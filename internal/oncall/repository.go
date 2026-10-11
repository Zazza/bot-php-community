// Package oncall — рубрика «Дежурство»: сессия-симулятор производственного инцидента
// (сбор предложений → голосование → развязка, 3 раунда → посмертный разбор).
// Все переходы фаз делает cron-тик по данным из БД — таймеров в памяти нет.
package oncall

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
)

// ErrSessionFinished — сессия уже закрыта (cancel/finish во время шага тика):
// переходы фаз по завершённой сессии отбрасываются.
var ErrSessionFinished = errors.New("смена дежурства уже завершена")

// SessionRow — строка oncall_sessions.
type SessionRow struct {
	ID             int64      `db:"id"`
	ChatID         int64      `db:"chat_id"`
	StartedAt      time.Time  `db:"started_at"`
	Round          int        `db:"round"`
	Phase          string     `db:"phase"`
	PhaseStarted   time.Time  `db:"phase_started"`
	PhaseDeadline  time.Time  `db:"phase_deadline"`
	PhaseMessageID int64      `db:"phase_message_id"`
	PostAttempts   int        `db:"post_attempts"`
	Title          string     `db:"title"`
	Scenario       string     `db:"scenario"`
	State          string     `db:"state"`
	SilentRounds   int        `db:"silent_rounds"`
	Status         string     `db:"status"`
	FinishedAt     *time.Time `db:"finished_at"`
}

// RoundRow — строка oncall_rounds.
type RoundRow struct {
	ID         int64     `db:"id"`
	SessionID  int64     `db:"session_id"`
	ChatID     int64     `db:"chat_id"`
	Round      int       `db:"round"`
	Title      string    `db:"title"`
	Scenario   string    `db:"scenario"`
	Plans      []byte    `db:"plans"`
	ChosenPlan int       `db:"chosen_plan"`
	Outcome    string    `db:"outcome"`
	CreatedAt  time.Time `db:"created_at"`
}

// ParsedPlans разбирает JSONB-колонку plans в срез планов.
func (r RoundRow) ParsedPlans() []Plan {
	if len(r.Plans) == 0 {
		return nil
	}
	var plans []Plan
	if err := json.Unmarshal(r.Plans, &plans); err != nil {
		return nil
	}
	return plans
}

// Repository читает/пишет сессии дежурства.
type Repository struct {
	db *sqlx.DB
}

// NewRepository создаёт repository.
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

// GetSession возвращает сессию по id. Не найдено — (nil, nil).
func (r *Repository) GetSession(ctx context.Context, id int64) (*SessionRow, error) {
	var row SessionRow
	if err := r.db.GetContext(ctx, &row, `
		SELECT id, chat_id, started_at, round, phase, phase_started, phase_deadline,
		       phase_message_id, post_attempts, title, scenario, state, silent_rounds, status, finished_at
		FROM oncall_sessions WHERE id = $1
	`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get oncall session: %w", err)
	}
	return &row, nil
}

// ActiveSession возвращает активную сессию чата. Нет активной — (nil, nil).
func (r *Repository) ActiveSession(ctx context.Context, chatID int64) (*SessionRow, error) {
	var row SessionRow
	if err := r.db.GetContext(ctx, &row, `
		SELECT id, chat_id, started_at, round, phase, phase_started, phase_deadline,
		       phase_message_id, post_attempts, title, scenario, state, silent_rounds, status, finished_at
		FROM oncall_sessions
		WHERE chat_id = $1 AND status = 'active'
		ORDER BY id DESC
		LIMIT 1
	`, chatID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("active oncall session: %w", err)
	}
	return &row, nil
}

// StartedToday — была ли в чате сессия, начавшаяся сегодня (локальная полночь).
func (r *Repository) StartedToday(ctx context.Context, chatID int64) (bool, error) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var exists bool
	if err := r.db.GetContext(ctx, &exists, `
		SELECT EXISTS(SELECT 1 FROM oncall_sessions WHERE chat_id = $1 AND started_at >= $2)
	`, chatID, midnight); err != nil {
		return false, fmt.Errorf("oncall started today: %w", err)
	}
	return exists, nil
}

// CreateSession пишет новую сессию в фазе brief (persist-then-post: контент и дедлайн
// сохраняются до постинга). Возвращает id. Гонка двух стартов в одном чате закрыта
// частичным unique-индексом uq_oncall_sessions_active (23505 → ErrActiveSession).
func (r *Repository) CreateSession(ctx context.Context, chatID int64, title, scenario string, phaseDeadline time.Time) (int64, error) {
	var id int64
	if err := r.db.GetContext(ctx, &id, `
		INSERT INTO oncall_sessions (chat_id, title, scenario, phase_deadline)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, chatID, title, scenario, phaseDeadline); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, ErrActiveSession
		}
		return 0, fmt.Errorf("create oncall session: %w", err)
	}
	return id, nil
}

// SetPhase переводит фазу и обнуляет phase_message_id (новый пост ещё не отправлен;
// тик увидит 0 и запостит сохранённый контент — resume после рестарта). Завершённая
// сессия не переводится (0 строк → ErrSessionFinished).
func (r *Repository) SetPhase(ctx context.Context, id int64, phase string, phaseStarted, phaseDeadline time.Time) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE oncall_sessions
		SET phase = $2, phase_started = $3, phase_deadline = $4, phase_message_id = 0
		WHERE id = $1 AND status = 'active'
	`, id, phase, phaseStarted, phaseDeadline)
	if err != nil {
		return fmt.Errorf("set oncall phase: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSessionFinished
	}
	return nil
}

// SetPhaseMessage привязывает id отправленного сообщения текущей фазы и сбрасывает
// счётчик неудачных постов (отправка прошла).
func (r *Repository) SetPhaseMessage(ctx context.Context, id, messageID int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE oncall_sessions SET phase_message_id = $2, post_attempts = 0 WHERE id = $1`, id, messageID); err != nil {
		return fmt.Errorf("set oncall phase message: %w", err)
	}
	return nil
}

// PostFailed инкрементит счётчик неудачных постов фазы (обе попытки отправки
// отвергнуты) и возвращает новое значение.
func (r *Repository) PostFailed(ctx context.Context, id int64) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n,
		`UPDATE oncall_sessions SET post_attempts = post_attempts + 1 WHERE id = $1 RETURNING post_attempts`,
		id); err != nil {
		return 0, fmt.Errorf("oncall post failed count: %w", err)
	}
	return n, nil
}

// SetScenario обновляет заголовок и вводную текущего раунда сессии.
// Завершённая сессия не обновляется (0 строк → ErrSessionFinished).
func (r *Repository) SetScenario(ctx context.Context, id int64, title, scenario string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE oncall_sessions SET title = $2, scenario = $3 WHERE id = $1 AND status = 'active'`, id, title, scenario)
	if err != nil {
		return fmt.Errorf("set oncall scenario: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSessionFinished
	}
	return nil
}

// SetState обновляет сводку метрик (передаётся в следующий раунд и в разбор).
func (r *Repository) SetState(ctx context.Context, id int64, state string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE oncall_sessions SET state = $2 WHERE id = $1`, id, state); err != nil {
		return fmt.Errorf("set oncall state: %w", err)
	}
	return nil
}

// SetRound перемещает указатель текущего раунда сессии. Завершённая сессия не
// обновляется (0 строк → ErrSessionFinished).
func (r *Repository) SetRound(ctx context.Context, id int64, round int) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE oncall_sessions SET round = $2 WHERE id = $1 AND status = 'active'`, id, round)
	if err != nil {
		return fmt.Errorf("set oncall round: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSessionFinished
	}
	return nil
}

// BumpSilent инкрементит счётчик тихих раундов подряд и возвращает новое значение.
// Условие фазы: пишется только после успешного перехода из brief (engine вызывает
// после SetPhase) — повторный вызов в одной и той же фазе невозможен, счётчик тишины
// не задваивается при сбойных ретраях тика.
func (r *Repository) BumpSilent(ctx context.Context, id int64) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n, `
		UPDATE oncall_sessions SET silent_rounds = silent_rounds + 1
		WHERE id = $1 AND phase = 'outcome'
		RETURNING silent_rounds
	`, id); err != nil {
		return 0, fmt.Errorf("bump oncall silent: %w", err)
	}
	return n, nil
}

// ResetSilent обнуляет счётчик тихих раундов (после «громкого» раунда).
func (r *Repository) ResetSilent(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE oncall_sessions SET silent_rounds = 0 WHERE id = $1`, id); err != nil {
		return fmt.Errorf("reset oncall silent: %w", err)
	}
	return nil
}

// Finish закрывает сессию (status='finished', finished_at=now()). verdict только
// логируется вызовом — колонки под него в схеме нет.
func (r *Repository) Finish(ctx context.Context, id int64, verdict string) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE oncall_sessions SET status = 'finished', finished_at = now() WHERE id = $1
	`, id); err != nil {
		return fmt.Errorf("finish oncall session: %w", err)
	}
	if verdict != "" {
		slog.Info("oncall session finished", "id", id, "verdict", verdict)
	}
	return nil
}

// InsertRound пишет раунд (идемпотентный upsert по (session_id, round) — повтор тика
// после сбоя не создаёт дубликат). Возвращает id.
func (r *Repository) InsertRound(ctx context.Context, sessID, chatID int64, round int, title, scenario string) (int64, error) {
	var id int64
	if err := r.db.GetContext(ctx, &id, `
		INSERT INTO oncall_rounds (session_id, chat_id, round, title, scenario)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (session_id, round) DO UPDATE SET title = EXCLUDED.title, scenario = EXCLUDED.scenario
		RETURNING id
	`, sessID, chatID, round, title, scenario); err != nil {
		return 0, fmt.Errorf("insert oncall round: %w", err)
	}
	return id, nil
}

// UpdateRoundPlans сохраняет JSONB-массив планов раунда.
func (r *Repository) UpdateRoundPlans(ctx context.Context, roundID int64, plansJSON []byte) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE oncall_rounds SET plans = $2 WHERE id = $1`, roundID, plansJSON); err != nil {
		return fmt.Errorf("update oncall round plans: %w", err)
	}
	return nil
}

// UpdateRoundResult фиксирует выбранный план и/или текст развязки (JSON Outcome).
func (r *Repository) UpdateRoundResult(ctx context.Context, roundID int64, chosen int, outcome string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE oncall_rounds SET chosen_plan = $2, outcome = $3 WHERE id = $1`,
		roundID, chosen, outcome); err != nil {
		return fmt.Errorf("update oncall round result: %w", err)
	}
	return nil
}

// RoundsOfSession возвращает все раунды сессии по порядку.
func (r *Repository) RoundsOfSession(ctx context.Context, sessID int64) ([]RoundRow, error) {
	var rows []RoundRow
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT id, session_id, chat_id, round, title, scenario, plans, chosen_plan, outcome, created_at
		FROM oncall_rounds WHERE session_id = $1 ORDER BY round
	`, sessID); err != nil {
		return nil, fmt.Errorf("oncall rounds of session: %w", err)
	}
	return rows, nil
}

// RoundByNumber возвращает раунд сессии по номеру. Не найдено — (nil, nil).
func (r *Repository) RoundByNumber(ctx context.Context, sessID int64, round int) (*RoundRow, error) {
	var row RoundRow
	if err := r.db.GetContext(ctx, &row, `
		SELECT id, session_id, chat_id, round, title, scenario, plans, chosen_plan, outcome, created_at
		FROM oncall_rounds WHERE session_id = $1 AND round = $2
	`, sessID, round); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("oncall round by number: %w", err)
	}
	return &row, nil
}

// RecentTitles возвращает заголовки недавних завязок чата (бан-лист генератору),
// свежие первыми.
func (r *Repository) RecentTitles(ctx context.Context, chatID int64, limit int) ([]string, error) {
	var rows []string
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT title FROM oncall_rounds
		WHERE chat_id = $1 AND title <> ''
		ORDER BY id DESC
		LIMIT $2
	`, chatID, limit); err != nil {
		return nil, fmt.Errorf("oncall recent titles: %w", err)
	}
	return rows, nil
}

// RecordBallot учитывает голос за план. Возвращает ok=false, если участник в этом
// раунде уже голосовал (ON CONFLICT DO NOTHING — один голос на юзера).
func (r *Repository) RecordBallot(ctx context.Context, sessID int64, round int, userID int64, planIdx int) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO oncall_ballots (session_id, round, user_id, plan_idx) VALUES ($1, $2, $3, $4)
		ON CONFLICT (session_id, round, user_id) DO NOTHING
	`, sessID, round, userID, planIdx)
	if err != nil {
		return false, fmt.Errorf("record oncall ballot: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// BallotCounts возвращает распределение голосов раунда по индексам планов.
func (r *Repository) BallotCounts(ctx context.Context, sessID int64, round int) (map[int]int, error) {
	var rows []struct {
		PlanIdx int `db:"plan_idx"`
		Cnt     int `db:"cnt"`
	}
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT plan_idx, count(*) AS cnt FROM oncall_ballots
		WHERE session_id = $1 AND round = $2 GROUP BY plan_idx
	`, sessID, round); err != nil {
		return nil, fmt.Errorf("oncall ballot counts: %w", err)
	}
	m := make(map[int]int, len(rows))
	for _, rr := range rows {
		m[rr.PlanIdx] = rr.Cnt
	}
	return m, nil
}
