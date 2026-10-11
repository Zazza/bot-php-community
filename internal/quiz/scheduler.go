package quiz

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
	"phpbot/internal/messages"
)

type window struct {
	start int
	end   int
}

func (w window) String() string {
	return fmt.Sprintf("%02d:00-%02d:00", w.start, w.end)
}

var defaultWindows = []window{{10, 13}, {15, 17}}

type schedCfg struct {
	silence    time.Duration
	windows    []window
	onCallDays []time.Weekday
}

var defaultSchedCfg = schedCfg{
	silence: 2 * time.Hour,
	windows: defaultWindows,
}

type Scheduler struct {
	q    *Quiz
	msgs *messages.Repository
	repo *Repository
	cfg  schedCfg
	cron *cron.Cron
}

func NewScheduler(q *Quiz, msgs *messages.Repository, repo *Repository, onCallDays []time.Weekday) *Scheduler {
	cfg := defaultSchedCfg
	cfg.onCallDays = onCallDays
	return &Scheduler{q: q, msgs: msgs, repo: repo, cfg: cfg}
}

func windowsFor(day time.Weekday, cfg schedCfg) []window {
	if len(cfg.windows) == 0 {
		return cfg.windows
	}
	for _, d := range cfg.onCallDays {
		if d == day {
			return cfg.windows[:1]
		}
	}
	return cfg.windows
}

func windowFor(hour int, ws []window) (window, bool) {
	for _, w := range ws {
		if hour >= w.start && hour < w.end {
			return w, true
		}
	}
	return window{}, false
}

func (s *Scheduler) Start(ctx context.Context, spec string) error {
	c := cron.New()
	_, err := c.AddFunc(spec, func() {
		bg, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		s.tick(bg)
	})
	if err != nil {
		return fmt.Errorf("add cron func: %w", err)
	}
	c.Start()
	s.cron = c
	days := make([]string, 0, len(s.cfg.onCallDays))
	for _, d := range s.cfg.onCallDays {
		days = append(days, d.String())
	}
	slog.Info("quiz scheduler started", "cron", spec, "silence", s.cfg.silence,
		"windows", s.cfg.windows, "oncall_days", days)
	return nil
}

func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now()
	for _, chatID := range s.q.chatIDs {
		if err := s.maybePost(ctx, chatID, now); err != nil {
			slog.Warn("quiz tick", "err", err, "chat_id", chatID)
		}
	}
}

func (s *Scheduler) maybePost(ctx context.Context, chatID int64, now time.Time) error {
	w, ok := windowFor(now.Hour(), windowsFor(now.Weekday(), s.cfg))
	if !ok {
		return nil
	}
	windowStart := time.Date(now.Year(), now.Month(), now.Day(), w.start, 0, 0, 0, now.Location())
	posted, err := s.repo.PostedSince(ctx, chatID, windowStart)
	if err != nil {
		return fmt.Errorf("posted since window start: %w", err)
	}
	recent, err := s.msgs.CountSince(ctx, chatID, now.Add(-s.cfg.silence))
	if err != nil {
		return fmt.Errorf("count recent: %w", err)
	}
	if !shouldPost(now, posted, recent == 0, s.cfg) {
		return nil
	}
	return s.q.Post(ctx, chatID)
}

// shouldPost — чистая функция решения, постить ли квиз сейчас: только в разрешённом
// окне дня, если в этом окне ещё не постили и чат тих (тишина-триггер: живая беседа
// важнее квиза, гарантийного поста нет).
func shouldPost(now time.Time, postedInWindow int, silent bool, cfg schedCfg) bool {
	_, ok := windowFor(now.Hour(), windowsFor(now.Weekday(), cfg))
	if !ok {
		return false
	}
	return postedInWindow == 0 && silent
}

func (s *Scheduler) Stop() {
	if s.cron != nil {
		s.cron.Stop()
	}
}
