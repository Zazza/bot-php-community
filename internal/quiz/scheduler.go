package quiz

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler ежедневно постит вопрос викторины в каждый чат.
type Scheduler struct {
	q    *Quiz
	cron *cron.Cron
}

// NewScheduler создаёт scheduler.
func NewScheduler(q *Quiz) *Scheduler { return &Scheduler{q: q} }

// Start запускает cron. Спецификация — стандартные 5 полей.
func (s *Scheduler) Start(ctx context.Context, spec string) error {
	c := cron.New()
	_, err := c.AddFunc(spec, func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, chatID := range s.q.chatIDs {
			if err := s.q.Post(bg, chatID); err != nil {
				slog.Warn("quiz cron post", "err", err, "chat_id", chatID)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("add cron func: %w", err)
	}
	c.Start()
	s.cron = c
	slog.Info("quiz scheduler started", "cron", spec)
	return nil
}

// Stop останавливает cron.
func (s *Scheduler) Stop() {
	if s.cron != nil {
		s.cron.Stop()
	}
}
