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

	"phpbot/internal/anniv"
	"phpbot/internal/announce"
	"phpbot/internal/chat"
	"phpbot/internal/config"
	"phpbot/internal/db"
	"phpbot/internal/faq"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/moderation"
	"phpbot/internal/news"
	"phpbot/internal/prompts"
	"phpbot/internal/quiz"
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
		"chats", cfg.ChatIDs, "admins", cfg.AdminIDs,
		"model", cfg.LLMModel, "model_cheap", cfg.LLMModelCheap,
		"embed_model", cfg.EmbedModel, "embed_dim", cfg.EmbedDim)

	// fail-fast: критичный промпт должен быть встроен.
	if prompts.Get(prompts.Judge) == "" {
		slog.Error("judge prompt missing in embed")
		os.Exit(1)
	}
	if cfg.SpamEnabled && prompts.Get(prompts.Spam) == "" {
		slog.Error("spam prompt missing in embed")
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

	llmClient := llm.NewLLMClient(cfg.LLMURL, cfg.LLMAPIKey, cfg.LLMModel, 2048, 0)     // умная: ответы /ask + модерация-судья + анти-спам (temp=0: детерминированно)
	llmCreative := llm.NewLLMClient(cfg.LLMURL, cfg.LLMAPIKey, cfg.LLMModel, 2048, 0.9) // умная+творческая temp=0.9: пятничный фейк-выпуск, генерация вопросов викторины
	llmCheap := llm.NewLLMClient(cfg.LLMURL, cfg.LLMAPIKey, cfg.LLMModelCheap, 2048, 0) // дешёвая: faq/digest/темы
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

	faqRepo := faq.NewRepo(dbx)
	faqBuilder := faq.NewBuilder(dbx, llmCheap, msgRepo, faqRepo, cfg.ChatIDs)

	answerer := chat.New(llmClient, msgRepo, vecRepo, webSearcher, faqRepo)
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
	moderFlow := moderation.NewFlow(b, llmClient, moderRepo, userRepo, msgRepo,
		cfg.GateEnabled, cfg.CaptchaTimeout, cfg.CaptchaMaxAttempts, cfg.Probation, cfg.AdminIDs)
	moderFlow.Start(ctx)
	defer moderFlow.Stop()

	spamFilter := moderation.NewSpamFilter(b, llmClient, moderRepo, msgRepo, cfg.AdminIDs, me.ID, moderation.SpamConfig{
		FloodMsgs: cfg.SpamFloodMsgs, FloodWindow: cfg.SpamFloodWindow,
		WarnMax: cfg.SpamWarnMax, WarnPeriod: cfg.SpamWarnPeriod, RestrictHours: cfg.SpamRestrictHours,
		NewbieMsgs: cfg.SpamNewbieMsgs, TrustMsgs: cfg.SpamTrustMsgs, EscalateEnabled: cfg.SpamEscalateEnabled,
	})
	if cfg.SpamEnabled {
		spamFilter.Start(ctx)
		defer spamFilter.Stop()
	}

	spamEsc := moderation.NewSpamEscalation(b, moderRepo, msgRepo, cfg.AdminIDs, me.ID, moderation.EscalationConfig{
		Enabled:   cfg.SpamEscalateEnabled,
		VoterMsgs: cfg.SpamVoterMsgs, EscalateSpam: cfg.SpamEscalateSpam, EscalateOk: cfg.SpamEscalateOk,
		RestrictHours: cfg.SpamRestrictHours,
	})
	if cfg.SpamEscalateEnabled {
		spamEsc.Start(ctx)
		defer spamEsc.Stop()
	}

	voteKick := moderation.NewVoteToKick(b, moderRepo, cfg.AdminIDs, me.ID, moderation.VoteConfig{
		Window: cfg.VoteWindow, Quorum: cfg.VoteQuorum,
	})
	if cfg.VoteEnabled {
		voteKick.Start(ctx)
		defer voteKick.Stop()
	}

	digester := topics.NewDigester(dbx, llmCheap, msgRepo, poster, cfg.ChatIDs)

	quizRepo := quiz.NewRepository(dbx)
	quizSvc := quiz.New(b, llmCreative, llmClient, quizRepo, cfg.ChatIDs)
	quizSched := quiz.NewScheduler(quizSvc, msgRepo, quizRepo)
	annivSched := anniv.New(msgRepo, llmClient, poster, cfg.ChatIDs)

	newsRepo := news.NewRepository(dbx)
	newsDigester := news.NewDigester(llmCheap, llmCreative, newsRepo, poster, cfg.ChatIDs, news.DefaultSources(), cfg.FakeNewsEnabled)

	announceSvc := announce.New(b, cfg.AdminIDs, cfg.ChatIDs[0])

	handlers := tg.NewHandlers(tg.HandlersDeps{
		API: b, ChatIDs: cfg.ChatIDs, BotUserID: me.ID,
		Moderation: moderFlow, Spam: spamFilter, Vote: voteKick, SpamEsc: spamEsc,
		Users: userRepo, Msgs: msgRepo, Vec: vecRepo,
		Answerer: answerer, Digester: digester,
		FAQ: faqRepo, FAQBuilder: faqBuilder, Quiz: quizSvc, News: newsDigester,
		Announce: announceSvc,
	})
	handlers.SetBotUsername(me.Username)

	router := tg.New(b, handlers)

	// Cron-шедулеры.
	if err := digester.Start(ctx, "0 9 * * 1"); err != nil {
		slog.Error("digest cron start", "err", err)
	}
	if err := faqBuilder.Start(ctx, "0 4 * * 1"); err != nil {
		slog.Error("faq cron start", "err", err)
	}
	if cfg.QuizEnabled {
		if err := quizSched.Start(ctx, cfg.QuizCheckCron); err != nil {
			slog.Error("quiz cron start", "err", err)
		}
		defer quizSched.Stop()
	}
	if cfg.NewsEnabled {
		if err := newsDigester.Start(ctx, cfg.NewsCron); err != nil {
			slog.Error("news cron start", "err", err)
		}
		defer newsDigester.Stop()
	}
	if cfg.AnniversaryEnabled {
		if err := annivSched.Start(ctx, cfg.AnniversaryCron); err != nil {
			slog.Error("anniversary cron start", "err", err)
		}
		defer annivSched.Stop()
	}
	defer digester.Stop()
	defer faqBuilder.Stop()

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
