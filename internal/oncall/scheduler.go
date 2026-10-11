package oncall

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// tickBudget — бюджет одного тика state machine: до 2 LLM-вызовов + посты.
const tickBudget = 5 * time.Minute

// Scheduler гоняет Engine.Tick по cron-расписанию. Все переходы фаз — внутри тика
// по данным из БД, поэтому частота тика задаёт только отзывчивость рубрики.
type Scheduler struct {
	mu   sync.Mutex
	eng  *Engine
	cron *cron.Cron
}

// NewScheduler создаёт планировщик.
func NewScheduler(eng *Engine) *Scheduler { return &Scheduler{eng: eng} }

// Start запускает тики. Каждый тик получает собственный контекст с бюджетом
// tickBudget (контекст шедулера в тики не протекает). robfig/cron гонит
// срабатывания в отдельных горутинах: пока предыдущий тик жив (до 2 LLM-вызовов),
// новый тихо пропускается (TryLock) — параллельные шаги state machine запрещены.
func (s *Scheduler) Start(ctx context.Context, spec string) error {
	c := cron.New()
	_, err := c.AddFunc(spec, func() {
		if !s.mu.TryLock() {
			slog.Debug("oncall tick skipped: previous tick still running")
			return
		}
		defer s.mu.Unlock()
		bg, cancel := context.WithTimeout(context.Background(), tickBudget)
		defer cancel()
		s.eng.Tick(bg)
	})
	if err != nil {
		return fmt.Errorf("add cron func: %w", err)
	}
	c.Start()
	s.cron = c
	slog.Info("oncall scheduler started", "cron", spec, "tick_budget", tickBudget)
	return nil
}

// Stop останавливает тики.
func (s *Scheduler) Stop() {
	if s.cron != nil {
		s.cron.Stop()
	}
}
