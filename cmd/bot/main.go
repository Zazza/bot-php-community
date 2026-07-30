// php-bot — модератор и ассистент TG-чата «PHP-сообщество Воронеж».
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"phpbot/internal/chat"
	"phpbot/internal/config"
	"phpbot/internal/db"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/moderation"
	"phpbot/internal/prompts"
	"phpbot/internal/tg"
	"phpbot/internal/topics"
	"phpbot/internal/users"
	"phpbot/internal/websearch"
)

func main() {
	setupLogger()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded",
		"chats", cfg.ChatIDs, "admins", cfg.AdminIDs, "model", cfg.LLMModel,
		"embed_model", cfg.EmbedModel, "embed_dim", cfg.EmbedDim)

	// fail-fast: критичный промпт должен быть встроен.
	if prompts.Get(prompts.Judge) == "" {
		slog.Error("judge prompt missing in embed")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbx, err := db.Connect(ctx, cfg.DBURL)
	if err != nil {
		slog.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer dbx.Close()

	llmClient := llm.NewLLMClient(cfg.LLMURL, cfg.LLMAPIKey, cfg.LLMModel, 2048)
	embedder := llm.NewEmbedder(cfg.LLMURL, cfg.LLMAPIKey, cfg.EmbedModel)

	msgRepo := messages.New(dbx)
	userRepo := users.New(dbx)
	vecRepo := messages.NewVectorRepo(dbx, embedder, 200)
	vecRepo.Start(ctx, 2)
	defer vecRepo.Stop()

	// Веб-поиск (SearXNG). Пустой URL → выключен (non-fatal).
	var webSearcher *websearch.Searcher
	if cfg.SearXNGURL != "" {
		webSearcher = websearch.New(cfg.SearXNGURL, cfg.SearXNGMax)
		slog.Info("web search enabled", "url", cfg.SearXNGURL, "max", cfg.SearXNGMax)
	}

	answerer := chat.New(llmClient, msgRepo, vecRepo, webSearcher)
	moderRepo := moderation.NewRepository(dbx)

	b, err := bot.New(cfg.TGToken, bot.WithDefaultHandler(func(_ context.Context, _ *bot.Bot, _ *models.Update) {}))
	if err != nil {
		slog.Error("tg bot init", "err", err)
		os.Exit(1)
	}

	me, err := b.GetMe(ctx)
	if err != nil {
		slog.Error("getMe", "err", err)
		os.Exit(1)
	}
	slog.Info("bot identity", "id", me.ID, "username", me.Username)

	poster := tg.NewPoster(b)
	moderFlow := moderation.NewFlow(b, llmClient, moderRepo, userRepo,
		cfg.GateEnabled, cfg.CaptchaTimeout, cfg.CaptchaMaxAttempts, cfg.Probation, cfg.AdminIDs)
	moderFlow.Start(ctx)
	defer moderFlow.Stop()
	topicsSched := topics.New(dbx, llmClient, msgRepo, poster, cfg.ChatIDs, cfg.QuietThreshold)
	digester := topics.NewDigester(dbx, llmClient, msgRepo, poster, cfg.ChatIDs)

	handlers := tg.NewHandlers(tg.HandlersDeps{
		API: b, ChatIDs: cfg.ChatIDs, BotUserID: me.ID,
		Moderation: moderFlow, Users: userRepo, Msgs: msgRepo, Vec: vecRepo,
		Answerer: answerer, Topics: topicsSched, Digester: digester,
	})
	handlers.SetBotUsername(me.Username)

	router := tg.New(b, handlers)

	// Cron-шедулеры.
	if err := topicsSched.Start(ctx, cfg.TopicCron); err != nil {
		slog.Error("topics cron start", "err", err)
	}
	if err := digester.Start(ctx, "0 9 * * 1"); err != nil {
		slog.Error("digest cron start", "err", err)
	}
	defer topicsSched.Stop()
	defer digester.Stop()

	// Graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutdown signal received")
		cancel()
	}()

	slog.Info("php-bot started")
	router.Start(ctx)
	slog.Info("php-bot stopped")
}

// setupLogger: slog в stdout + файл (если PHPBOT_LOG_PATH задан).
func setupLogger() {
	writers := []io.Writer{os.Stdout}
	if path := os.Getenv("PHPBOT_LOG_PATH"); path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			writers = append(writers, f)
		} else {
			slog.Warn("log file open failed, stdout only", "err", err, "path", path)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
}
