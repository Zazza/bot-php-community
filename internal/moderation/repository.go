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
