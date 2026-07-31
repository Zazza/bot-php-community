// Package users — tracking участников чата: статус (member/suspect/banned), бан-лог.
package users

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// User — строка таблицы users.
type User struct {
	TGUserID  int64     `db:"tg_user_id"`
	Username  string    `db:"username"`
	FirstSeen time.Time `db:"first_seen"`
	Status    string    `db:"status"` // member | suspect | banned
}

// Repository читает/пишет пользователей.
type Repository struct {
	db *sqlx.DB
}

// New создаёт repository.
func New(db *sqlx.DB) *Repository { return &Repository{db: db} }

// Upsert создаёт или обновляет запись о пользователе. Возвращает текущий статус.
func (r *Repository) Upsert(ctx context.Context, u *User) (status string, err error) {
	var s string
	err = r.db.GetContext(ctx, &s, `
		INSERT INTO users (tg_user_id, username, first_seen, status)
		VALUES ($1, $2, now(), COALESCE($3, 'member'))
		ON CONFLICT (tg_user_id) DO UPDATE
			SET username = EXCLUDED.username
		RETURNING status
	`, u.TGUserID, u.Username, u.Status)
	if err != nil {
		return "", fmt.Errorf("upsert user: %w", err)
	}
	return s, nil
}

// Get возвращает пользователя.
func (r *Repository) Get(ctx context.Context, tgUserID int64) (*User, error) {
	var u User
	if err := r.db.GetContext(ctx, &u,
		`SELECT tg_user_id, username, first_seen, status FROM users WHERE tg_user_id = $1`,
		tgUserID); err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return &u, nil
}

// SetStatus меняет статус (member/suspect/banned).
func (r *Repository) SetStatus(ctx context.Context, tgUserID int64, status string) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE users SET status = $2 WHERE tg_user_id = $1`,
		tgUserID, status); err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	return nil
}

// IsSuspect — новичок ещё не проверен (статус suspect).
func (r *Repository) IsSuspect(ctx context.Context, tgUserID int64) (bool, error) {
	u, err := r.Get(ctx, tgUserID)
	if err != nil {
		return false, err
	}
	return u.Status == "suspect", nil
}

// MarkBanned помечает пользователя забаненным.
func (r *Repository) MarkBanned(ctx context.Context, tgUserID int64) error {
	return r.SetStatus(ctx, tgUserID, "banned")
}

// TouchUser запоминает @handle пользователя (для разрешения @username → user_id)
// и лениво нормализует историю: импортированные сообщения хранили display-name,
// а не @handle — проставляем реальный @handle по user_id. Статус/first_seen не трогаем.
func (r *Repository) TouchUser(ctx context.Context, userID int64, username string) error {
	username = strings.TrimSpace(username)
	if userID == 0 || username == "" {
		return nil
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO users (tg_user_id, username) VALUES ($1, $2)
		ON CONFLICT (tg_user_id) DO UPDATE
			SET username = EXCLUDED.username
			WHERE users.username <> EXCLUDED.username
	`, userID, username); err != nil {
		return fmt.Errorf("touch user: %w", err)
	}
	var need bool
	if err := r.db.GetContext(ctx, &need,
		`SELECT EXISTS (SELECT 1 FROM messages WHERE user_id = $1 AND username <> $2 LIMIT 1)`,
		userID, username); err != nil {
		return fmt.Errorf("touch user check: %w", err)
	}
	if !need {
		return nil
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE messages SET username = $2 WHERE user_id = $1 AND username <> $2`,
		userID, username); err != nil {
		return fmt.Errorf("touch user backfill: %w", err)
	}
	slog.Info("user history backfill", "user_id", userID, "username", username)
	return nil
}
