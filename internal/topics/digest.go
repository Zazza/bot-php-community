package topics

import (
	"context"
	"errors"
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

// ErrTooFewMessages — дайджест не строится: за период слишком мало сообщений.
// Cron трактует как не-ошибку (debug), ручная команда /digest — сообщает пользователю.
var ErrTooFewMessages = errors.New("too few messages for digest")

// minDigestMessages — минимальное число сообщений за период, чтобы дайджест имел смысл.
const minDigestMessages = 3

// Digester делает недельную суммаризацию чата.
type Digester struct {
	db      *sqlx.DB
	llm     *llm.LLMClient
	msgs    *messages.Repository
	api     Poster
	chatIDs []int64
	cron    *cron.Cron
}

// NewDigester создаёт Digester.
func NewDigester(db *sqlx.DB, llm *llm.LLMClient, msgs *messages.Repository, api Poster, chatIDs []int64) *Digester {
	return &Digester{db: db, llm: llm, msgs: msgs, api: api, chatIDs: chatIDs}
}

// Start регистрирует еженедельный отчёт (по умолчанию пн 09:00) и запускает cron.
func (d *Digester) Start(ctx context.Context, spec string) error {
	c := cron.New()
	_, err := c.AddFunc(spec, func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		end := time.Now()
		start := end.Add(-7 * 24 * time.Hour)
		for _, chatID := range d.chatIDs {
			if err := d.PostDigest(bg, chatID, chatID, start, end); err != nil {
				if errors.Is(err, ErrTooFewMessages) {
					slog.Debug("weekly digest skipped", "chat_id", chatID, "err", err)
				} else {
					slog.Error("weekly digest", "chat_id", chatID, "err", err)
				}
			}
		}
	})
	if err != nil {
		return fmt.Errorf("add digest cron: %w", err)
	}
	c.Start()
	d.cron = c
	slog.Info("digest scheduler started", "cron", spec)
	return nil
}

// Stop останавливает cron.
func (d *Digester) Stop() {
	if d.cron != nil {
		d.cron.Stop()
	}
}

// PostDigest суммаризирует сообщения за [start, end), постит в чат, пишет topic_digests.
// dataChatID — чат-источник данных (выборка); postChatID — куда постить. В группе
// они совпадают; в ЛС (админ) postChatID = ЛС, dataChatID = группа.
func (d *Digester) PostDigest(ctx context.Context, dataChatID, postChatID int64, start, end time.Time) error {
	msgs, err := d.msgs.Since(ctx, dataChatID, start, end)
	if err != nil {
		return fmt.Errorf("since: %w", err)
	}
	if len(msgs) < minDigestMessages {
		slog.Info("digest: too few messages, skip", "chat_id", dataChatID, "count", len(msgs))
		return fmt.Errorf("%w: have %d, need %d", ErrTooFewMessages, len(msgs), minDigestMessages)
	}
	material := chatFormatForDigest(msgs)
	prompt := prompts.Get(prompts.Digest, material)
	resp, _, _, err := d.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Сделай недельный дайджест."},
	})
	if err != nil {
		return fmt.Errorf("digest llm: %w", err)
	}
	summary := strings.TrimSpace(resp)

	header := fmt.Sprintf("📋 Дайджест недели (%s — %s)\n\n",
		start.Format("02.01"), end.Add(-24*time.Hour).Format("02.01"))
	if err := d.api.PostMessage(ctx, postChatID, header+summary); err != nil {
		return fmt.Errorf("post digest: %w", err)
	}
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO topic_digests (chat_id, period_start, period_end, summary, posted)
		VALUES ($1, $2, $3, $4, TRUE)
	`, dataChatID, start, end, summary); err != nil {
		slog.Warn("save digest row", "err", err)
	}
	return nil
}

// chatFormatForDigest — хронологический текст сообщений для подачи в LLM.
func chatFormatForDigest(msgs []messages.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		who := m.Username
		if who == "" {
			who = "user"
		}
		ts := m.TS.Format("02.01 15:04")
		text := m.Text
		if len(text) > 500 {
			text = text[:500] + "…"
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", ts, who, text)
	}
	return b.String()
}
