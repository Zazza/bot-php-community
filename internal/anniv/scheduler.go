// Package anniv — ежедневное поздравление участников с годовщиной в чате по users.first_seen.
package anniv

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
	"phpbot/internal/users"
)

// Poster — минимальный интерфейс отправки (удовлетворяется tg.PosterImpl структурно).
type Poster interface {
	PostMessage(ctx context.Context, chatID int64, text string) error
}

// Scheduler ежедневно поздравляет именинников.
type Scheduler struct {
	users   *users.Repository
	api     Poster
	chatIDs []int64
	cron    *cron.Cron
}

// New создаёт scheduler.
func New(usersRepo *users.Repository, api Poster, chatIDs []int64) *Scheduler {
	return &Scheduler{users: usersRepo, api: api, chatIDs: chatIDs}
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
	us, err := s.users.Anniversaries(ctx, now)
	if err != nil {
		slog.Error("anniversary query", "err", err)
		return
	}
	if len(us) == 0 {
		return
	}
	for _, chatID := range s.chatIDs {
		for _, u := range us {
			years := now.Year() - u.FirstSeen.Year()
			if years <= 0 {
				continue
			}
			handle := u.Username
			if handle == "" {
				handle = "участник"
			} else {
				handle = "@" + handle
			}
			text := fmt.Sprintf("🎉 %s уже %d %s с нами в «PHP-сообществе Воронеж»! Спасибо, что рядом 🙌",
				handle, years, pluralYears(years))
			if err := s.api.PostMessage(ctx, chatID, text); err != nil {
				slog.Warn("anniversary post", "err", err, "chat_id", chatID)
			}
		}
	}
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
