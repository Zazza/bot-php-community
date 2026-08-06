// Package anniv — ежедневное поздравление участников с годовщиной первого сообщения в чате.
package anniv

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/prompts"
)

// maxCardLen ограничивает длину LLM-посвящения, чтобы пост не разрастался.
const maxCardLen = 600

// Poster — минимальный интерфейс отправки (удовлетворяется tg.PosterImpl структурно).
type Poster interface {
	PostMessage(ctx context.Context, chatID int64, text string) error
}

// Scheduler ежедневно поздравляет именинников.
type Scheduler struct {
	msgs    *messages.Repository
	llm     *llm.LLMClient
	api     Poster
	chatIDs []int64
	cron    *cron.Cron
}

// New создаёт scheduler. llm — умная модель (посвящение — публичный пост о реальном человеке).
func New(msgs *messages.Repository, llm *llm.LLMClient, api Poster, chatIDs []int64) *Scheduler {
	return &Scheduler{msgs: msgs, llm: llm, api: api, chatIDs: chatIDs}
}

// Start запускает cron. Спецификация — стандартные 5 полей.
func (s *Scheduler) Start(ctx context.Context, spec string) error {
	c := cron.New()
	_, err := c.AddFunc(spec, func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.run(bg)
	})
	if err != nil {
		return fmt.Errorf("add cron func: %w", err)
	}
	c.Start()
	s.cron = c
	slog.Info("anniversary scheduler started", "cron", spec)
	return nil
}

// Stop останавливает cron.
func (s *Scheduler) Stop() {
	if s.cron != nil {
		s.cron.Stop()
	}
}

func (s *Scheduler) run(ctx context.Context) {
	now := time.Now()
	for _, chatID := range s.chatIDs {
		us, err := s.msgs.Anniversaries(ctx, chatID, now)
		if err != nil {
			slog.Error("anniversary query", "err", err, "chat_id", chatID)
			continue
		}
		for _, u := range us {
			years := now.Year() - u.First.Year()
			if years <= 0 {
				continue
			}
			name := displayName(u.Username)
			card := s.card(ctx, chatID, u.UserID, name, years)
			text := announce(name, years, card)
			if err := s.api.PostMessage(ctx, chatID, text); err != nil {
				slog.Warn("anniversary post", "err", err, "chat_id", chatID)
			}
		}
	}
}

// announce собирает итоговый пост: заголовок-поздравление + LLM-посвящение (если есть).
func announce(name string, years int, card string) string {
	head := fmt.Sprintf("🎉 **%s** уже %d %s с нами! 🎂", name, years, pluralYears(years))
	card = strings.TrimSpace(card)
	if card == "" {
		return head
	}
	return head + "\n\n" + card
}

// displayName решает, как показать участника: настоящий @handle → с @ (пингует),
// иначе — отображаемое имя без @ (раньше @ лепился на кириллическое имя → кривой «@Ольга Радевская»).
func displayName(u string) string {
	if u == "" || u == "user" {
		return "участник"
	}
	if looksLikeHandle(u) {
		return "@" + u
	}
	return u
}

// card генерирует LLM-посвящение по статистике и выборке сообщений. При любой ошибке/пустоте —
// "" (fail-safe: пост выйдет без карточки, но поздравление не теряется).
func (s *Scheduler) card(ctx context.Context, chatID, userID int64, name string, years int) string {
	if s.llm == nil {
		return ""
	}
	data := s.buildData(ctx, chatID, userID, name, years)
	resp, _, _, err := s.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompts.Get(prompts.Anniv)},
		{Role: "user", Content: data},
	})
	if err != nil {
		slog.Warn("anniversary llm", "err", err, "user_id", userID)
		return ""
	}
	resp = strings.TrimSpace(resp)
	if len(resp) > maxCardLen {
		resp = resp[:maxCardLen] + "…"
	}
	return resp
}

// buildData собирает данные участника для промпта: стаж + статистика + ранние/недавние сообщения.
func (s *Scheduler) buildData(ctx context.Context, chatID, userID int64, name string, years int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Участник: %s\n", name)
	fmt.Fprintf(&b, "Стаж: %d %s\n", years, pluralYears(years))
	if st, err := s.msgs.UserStats(ctx, chatID, userID); err == nil {
		fmt.Fprintf(&b, "Сообщений: %d\n", st.Count)
		if st.CodeMsgs > 0 {
			fmt.Fprintf(&b, "Сообщений с кодом: %d\n", st.CodeMsgs)
		}
		fmt.Fprintf(&b, "Средняя длина: %.0f симв.\n", st.AvgLen)
		if st.PeakHour >= 0 {
			fmt.Fprintf(&b, "Пик активности: %02d:00–%02d:00\n", st.PeakHour, (st.PeakHour+1)%24)
		}
	}
	oldest, _ := s.msgs.FirstByUser(ctx, userID, 8)
	recent, _ := s.msgs.LastByUser(ctx, userID, 8)
	b.WriteString("\n[Ранние сообщения]\n")
	writeSample(&b, oldest)
	b.WriteString("\n[Недавние сообщения]\n")
	writeSample(&b, recent)
	return b.String()
}

func writeSample(b *strings.Builder, ms []messages.Message) {
	for _, m := range ms {
		line := strings.ReplaceAll(m.Text, "\n", " ")
		if len(line) > 140 {
			line = line[:140] + "…"
		}
		fmt.Fprintf(b, "[%s] %s\n", m.TS.Format("02.01.2006"), line)
	}
}

// looksLikeHandle — похоже ли значение на настоящий TG @username (5–32 символа из [A-Za-z0-9_]).
func looksLikeHandle(s string) bool {
	if len(s) < 5 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// pluralYears — склонение «год/года/лет».
func pluralYears(n int) string {
	if n%100 >= 11 && n%100 <= 14 {
		return "лет"
	}
	switch n % 10 {
	case 1:
		return "год"
	case 2, 3, 4:
		return "года"
	default:
		return "лет"
	}
}
