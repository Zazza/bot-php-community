package quiz

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
	"phpbot/internal/messages"
)

// Параметры тишина-триггера викторины.
type schedCfg struct {
	silence      time.Duration // тишина чата, достаточная для поста
	minInterval  time.Duration // мин. интервал между квизами
	dailyCap     int           // максимум квизов в день на чат
	windowStart  int           // час начала окна постинга (включительно)
	windowEnd    int           // час конца окна (эксклюзивно)
	fallbackFrom int           // с этого часа — fallback-пост, если за день ещё не постили
}

var defaultSchedCfg = schedCfg{
	silence:      2 * time.Hour,
	minInterval:  4 * time.Hour,
	dailyCap:     2,
	windowStart:  10,
	windowEnd:    22,
	fallbackFrom: 21,
}

// Scheduler постит викторину в моменты тишины чата (заменяет фиксированное время и роль
// убранных «тем для разговора»): до dailyCap квизов в день, с мин. интервалом, в окне
// windowStart–windowEnd; если за день не постили — fallback ближе к вечеру.
type Scheduler struct {
	q    *Quiz
	msgs *messages.Repository
	repo *Repository
	cfg  schedCfg
	cron *cron.Cron
}

// NewScheduler создаёт scheduler тишина-триггера.
func NewScheduler(q *Quiz, msgs *messages.Repository, repo *Repository) *Scheduler {
	return &Scheduler{q: q, msgs: msgs, repo: repo, cfg: defaultSchedCfg}
}

// Start запускает чекер по spec (период проверки, обычно каждые 15 минут).
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
	slog.Info("quiz scheduler started", "cron", spec, "silence", s.cfg.silence,
		"min_interval", s.cfg.minInterval, "cap", s.cfg.dailyCap)
	return nil
}

// tick проверяет все чаты и постит викторину там, где выполнены условия.
func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now()
	for _, chatID := range s.q.chatIDs {
		if err := s.maybePost(ctx, chatID, now); err != nil {
			slog.Warn("quiz tick", "err", err, "chat_id", chatID)
		}
	}
}

// maybePost постит викторину в чат, если решит shouldPost; иначе ничего не делает.
func (s *Scheduler) maybePost(ctx context.Context, chatID int64, now time.Time) error {
	posted, err := s.repo.PostedToday(ctx, chatID)
	if err != nil {
		return err
	}
	lastPost, hasPost, err := s.repo.LastPostAt(ctx, chatID)
	if err != nil {
		return err
	}
	recent, err := s.msgs.CountSince(ctx, chatID, now.Add(-s.cfg.silence))
	if err != nil {
		return err
	}
	if !shouldPost(now, lastPost, hasPost, posted, recent == 0, s.cfg) {
		return nil
	}
	return s.q.Post(ctx, chatID)
}

// shouldPost — чистая функция решения, постить ли квиз сейчас. now — текущее время,
// lastPost/hasPost — последний квиз в чате, postedToday — сколько уже постили сегодня,
// silent — тих ли чат (нет сообщений за cfg.silence).
func shouldPost(now, lastPost time.Time, hasPost bool, postedToday int, silent bool, cfg schedCfg) bool {
	if postedToday >= cfg.dailyCap {
		return false
	}
	hour := now.Hour()
	if hour < cfg.windowStart || hour >= cfg.windowEnd {
		return false
	}
	if hasPost && now.Sub(lastPost) < cfg.minInterval {
		return false
	}
	fallback := postedToday == 0 && hour >= cfg.fallbackFrom
	return silent || fallback
}

// Stop останавливает cron.
func (s *Scheduler) Stop() {
	if s.cron != nil {
		s.cron.Stop()
	}
}
