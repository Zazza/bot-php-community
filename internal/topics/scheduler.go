// Package topics — генерация тем для оживления чата, шедулер, постинг в тишину.
package topics

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/robfig/cron/v3"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/prompts"
)

// Scheduler управляет темами: cron-проверка тишины чата, генерация, постинг.
type Scheduler struct {
	db            *sqlx.DB
	llm           *llm.LLMClient
	msgs          *messages.Repository
	api           Poster           // абстракция над TG (чтобы не тянуть bot.Bot в тестах)
	chatIDs       []int64
	quietThreshold int
	cron          *cron.Cron
}

// Poster — минимальный интерфейс для постинга в чат (реализует обёртка tg).
type Poster interface {
	PostMessage(ctx context.Context, chatID int64, text string) error
}

// New создаёт Scheduler.
func New(db *sqlx.DB, llm *llm.LLMClient, msgs *messages.Repository, api Poster,
	chatIDs []int64, quietThreshold int) *Scheduler {
	return &Scheduler{
		db: db, llm: llm, msgs: msgs, api: api,
		chatIDs:        chatIDs,
		quietThreshold: quietThreshold,
	}
}

// Start запускает cron-расписание проверки тишины.
func (s *Scheduler) Start(ctx context.Context, spec string) error {
	c := cron.New()
	_, err := c.AddFunc(spec, func() {
		s.checkQuietAndPost(ctx)
	})
	if err != nil {
		return fmt.Errorf("add cron func: %w", err)
	}
	c.Start()
	s.cron = c
	slog.Info("topics scheduler started", "cron", spec)
	return nil
}

// Stop останавливает cron.
func (s *Scheduler) Stop() {
	if s.cron != nil {
		s.cron.Stop()
	}
}

// checkQuietAndPost — логика cron-тика: для каждого чата смотрим активность,
// если тихо — берём/генерируем топик и постим.
func (s *Scheduler) checkQuietAndPost(ctx context.Context) {
	since := time.Now().Add(-24 * time.Hour)
	for _, chatID := range s.chatIDs {
		n, err := s.msgs.CountSince(ctx, chatID, since)
		if err != nil {
			slog.Error("count since", "chat_id", chatID, "err", err)
			continue
		}
		if n >= s.quietThreshold {
			continue // чат живой
		}
		topic, err := s.nextOrGenerate(ctx)
		if err != nil {
			slog.Error("get topic", "err", err)
			continue
		}
		if err := s.post(ctx, chatID, topic); err != nil {
			slog.Error("post topic", "err", err)
		}
	}
}

// PostNow — ручная команда /topic now: генерирует свежий топик и постит сразу.
func (s *Scheduler) PostNow(ctx context.Context, chatID int64) (string, error) {
	topic, err := s.Generate(ctx)
	if err != nil {
		return "", err
	}
	if err := s.post(ctx, chatID, topic); err != nil {
		return "", err
	}
	return topic, nil
}

// post отправляет топик в чат и помечает used.
func (s *Scheduler) post(ctx context.Context, chatID int64, text string) error {
	if err := s.api.PostMessage(ctx, chatID, "🗣 Тема для обсуждения:\n\n"+text); err != nil {
		return fmt.Errorf("post: %w", err)
	}
	// Помечаем последний использованный топик (по text) как used.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE topics SET used = TRUE, posted_at = now()
		WHERE text = $1 AND used = FALSE
	`, text); err != nil {
		slog.Warn("mark topic used", "err", err)
	}
	return nil
}

// nextOrGenerate: берёт свободный топик из БД, иначе генерит новый через LLM.
func (s *Scheduler) nextOrGenerate(ctx context.Context) (string, error) {
	var text string
	err := s.db.GetContext(ctx, &text, `
		SELECT text FROM topics WHERE used = FALSE ORDER BY id LIMIT 1
	`)
	if err == nil {
		return text, nil
	}
	// sql: no rows → generate. Любая другая ошибка — тоже генерим, не падаем.
	return s.Generate(ctx)
}

// Generate просит LLM выдать новый топик, избегая повторов с уже использованными.
func (s *Scheduler) Generate(ctx context.Context) (string, error) {
	used, _ := s.recentUsed(ctx, 10)
	usedBlock := "(пока нет)"
	if len(used) > 0 {
		usedBlock = strings.Join(used, "\n")
	}
	prompt := prompts.Get(prompts.Topic, usedBlock)
	resp, _, _, err := s.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Сгенерируй тему."},
	})
	if err != nil {
		return "", fmt.Errorf("generate topic: %w", err)
	}
	topic := strings.TrimSpace(resp)
	topic = strings.Trim(topic, "\"'«»")
	if topic == "" {
		return "", fmt.Errorf("empty topic")
	}
	// Сохраняем в библиотеку.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO topics (text, source) VALUES ($1, 'llm')`, topic); err != nil {
		slog.Warn("save generated topic", "err", err)
	}
	return topic, nil
}

// recentUsed возвращает последние использованные темы (для anti-repeat).
func (s *Scheduler) recentUsed(ctx context.Context, limit int) ([]string, error) {
	var rows []string
	if err := s.db.SelectContext(ctx, &rows, `
		SELECT text FROM topics WHERE used = TRUE ORDER BY posted_at DESC NULLS LAST LIMIT $1
	`, limit); err != nil {
		return nil, err
	}
	return rows, nil
}
