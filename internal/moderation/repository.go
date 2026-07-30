package moderation

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// VerdictRecord — строка таблицы newcomer_verdicts.
type VerdictRecord struct {
	ID          int64      `db:"id"`
	TGUserID    int64      `db:"tg_user_id"`
	ChatID      int64      `db:"chat_id"`
	Question    string     `db:"question"`
	Answer      string     `db:"answer"`
	Verdict     string     `db:"verdict"`
	Reason      string     `db:"reason"`
	AdminAction *string    `db:"admin_action"` // kicked | kept | NULL
	CreatedAt   time.Time  `db:"created_at"`
	DecidedAt   *time.Time `db:"decided_at"`
}

// Repository хранит вердикты модерации.
type Repository struct {
	db *sqlx.DB
}

// NewRepository создаёт repository.
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

// SaveVerdict записывает вердикт. Возвращает id для привязки inline-keyboard callback.
func (r *Repository) SaveVerdict(ctx context.Context, rec *VerdictRecord) (int64, error) {
	var id int64
	err := r.db.GetContext(ctx, &id, `
		INSERT INTO newcomer_verdicts (tg_user_id, chat_id, question, answer, verdict, reason)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, rec.TGUserID, rec.ChatID, rec.Question, rec.Answer, rec.Verdict, rec.Reason)
	if err != nil {
		return 0, fmt.Errorf("save verdict: %w", err)
	}
	return id, nil
}

// SetAdminAction отмечает решение админа (kicked/kept) и проставляет decided_at.
func (r *Repository) SetAdminAction(ctx context.Context, id int64, action string) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE newcomer_verdicts
		SET admin_action = $2, decided_at = now()
		WHERE id = $1
	`, id, action); err != nil {
		return fmt.Errorf("set admin action: %w", err)
	}
	return nil
}

// Get возвращает вердикт по id.
func (r *Repository) Get(ctx context.Context, id int64) (*VerdictRecord, error) {
	var v VerdictRecord
	if err := r.db.GetContext(ctx, &v, `
		SELECT id, tg_user_id, chat_id, question, answer, verdict, reason,
		       admin_action, created_at, decided_at
		FROM newcomer_verdicts WHERE id = $1
	`, id); err != nil {
		return nil, fmt.Errorf("get verdict: %w", err)
	}
	return &v, nil
}

// --- гейт новичков (newcomer_gates) ---

// GateRecord — строка newcomer_gates.
type GateRecord struct {
	ID               int64      `db:"id"`
	ChatID           int64      `db:"chat_id"`
	TGUserID         int64      `db:"tg_user_id"`
	Username         string     `db:"username"`
	CaptchaMessageID int64      `db:"captcha_message_id"`
	CorrectOption    int        `db:"correct_option"`
	Attempts         int        `db:"attempts"`
	State            string     `db:"state"` // pending | solved | kicked | cancelled
	JoinedAt         time.Time  `db:"joined_at"`
	Deadline         time.Time  `db:"deadline"`
	ProbationUntil   *time.Time `db:"probation_until"`
	ReleasedAt       *time.Time `db:"released_at"`
}

const gateCols = `id, chat_id, tg_user_id, username, captcha_message_id, correct_option,
	attempts, state, joined_at, deadline, probation_until, released_at`

// CreateGate создаёт запись гейта, возвращает id для привязки кнопок.
func (r *Repository) CreateGate(ctx context.Context, chatID, userID int64, username string, correct int, deadline time.Time) (int64, error) {
	var id int64
	err := r.db.GetContext(ctx, &id, `
		INSERT INTO newcomer_gates (chat_id, tg_user_id, username, correct_option, deadline)
		VALUES ($1, $2, $3, $4, $5) RETURNING id
	`, chatID, userID, username, correct, deadline)
	if err != nil {
		return 0, fmt.Errorf("create gate: %w", err)
	}
	return id, nil
}

// SetCaptchaMessage привязывает id отправленного сообщения с капчей.
func (r *Repository) SetCaptchaMessage(ctx context.Context, id, messageID int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE newcomer_gates SET captcha_message_id = $2 WHERE id = $1`, id, messageID); err != nil {
		return fmt.Errorf("set captcha message: %w", err)
	}
	return nil
}

// GetGate возвращает запись гейта по id.
func (r *Repository) GetGate(ctx context.Context, id int64) (*GateRecord, error) {
	var g GateRecord
	if err := r.db.GetContext(ctx, &g,
		`SELECT `+gateCols+` FROM newcomer_gates WHERE id = $1`, id); err != nil {
		return nil, fmt.Errorf("get gate: %w", err)
	}
	return &g, nil
}

// IncAttempts инкрементирует счётчик неверных попыток, возвращает новое значение.
func (r *Repository) IncAttempts(ctx context.Context, id int64) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n,
		`UPDATE newcomer_gates SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts`, id); err != nil {
		return 0, fmt.Errorf("inc attempts: %w", err)
	}
	return n, nil
}

// SetGateSolved помечает капчу решённой и ставит срок линк-провации.
func (r *Repository) SetGateSolved(ctx context.Context, id int64, probationUntil time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE newcomer_gates SET state = 'solved', probation_until = $2 WHERE id = $1`, id, probationUntil); err != nil {
		return fmt.Errorf("set gate solved: %w", err)
	}
	return nil
}

// SetGateKicked / SetGateCancelled — терминальные состояния.
func (r *Repository) SetGateKicked(ctx context.Context, id int64) error {
	return r.setGateState(ctx, id, "kicked")
}
func (r *Repository) SetGateCancelled(ctx context.Context, id int64) error {
	return r.setGateState(ctx, id, "cancelled")
}
func (r *Repository) setGateState(ctx context.Context, id int64, state string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE newcomer_gates SET state = $2 WHERE id = $1`, id, state); err != nil {
		return fmt.Errorf("set gate state: %w", err)
	}
	return nil
}

// SetGateReleased отмечает снятие линк-провации (released_at), состояние остаётся solved.
func (r *Repository) SetGateReleased(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE newcomer_gates SET released_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("set gate released: %w", err)
	}
	return nil
}

// PendingDue — просроченные pending (дедлайн прошёл) → кик.
func (r *Repository) PendingDue(ctx context.Context, now time.Time) ([]GateRecord, error) {
	var gs []GateRecord
	if err := r.db.SelectContext(ctx, &gs,
		`SELECT `+gateCols+` FROM newcomer_gates WHERE state = 'pending' AND deadline < $1`, now); err != nil {
		return nil, fmt.Errorf("pending due: %w", err)
	}
	return gs, nil
}

// PendingNotDue — pending, чей дедлайн ещё не прошёл (для cleanup-проверки через getChatMember).
func (r *Repository) PendingNotDue(ctx context.Context, now time.Time) ([]GateRecord, error) {
	var gs []GateRecord
	if err := r.db.SelectContext(ctx, &gs,
		`SELECT `+gateCols+` FROM newcomer_gates WHERE state = 'pending' AND deadline >= $1`, now); err != nil {
		return nil, fmt.Errorf("pending not due: %w", err)
	}
	return gs, nil
}

// ProbationDue — решённые с истёкшей линк-провацией, ограничение ещё не снято.
func (r *Repository) ProbationDue(ctx context.Context, now time.Time) ([]GateRecord, error) {
	var gs []GateRecord
	if err := r.db.SelectContext(ctx, &gs,
		`SELECT `+gateCols+` FROM newcomer_gates
		WHERE state = 'solved' AND probation_until IS NOT NULL AND probation_until < $1 AND released_at IS NULL`, now); err != nil {
		return nil, fmt.Errorf("probation due: %w", err)
	}
	return gs, nil
}

// PendingGateForUser — активная капча пользователя в чате (для cleanup при уходе).
// Не найдено → (nil, nil).
func (r *Repository) PendingGateForUser(ctx context.Context, chatID, userID int64) (*GateRecord, error) {
	var g GateRecord
	if err := r.db.GetContext(ctx, &g,
		`SELECT `+gateCols+` FROM newcomer_gates
		WHERE chat_id = $1 AND tg_user_id = $2 AND state = 'pending'
		ORDER BY id DESC LIMIT 1`, chatID, userID); err != nil {
		return nil, nil
	}
	return &g, nil
}
