// Package faq — курируемые вопрос-ответы: репозиторий faq_items и офлайн-билдер
// канон-ответов из кластеров вопросо-подобных сообщений чата.
package faq

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/pgvector/pgvector-go"
)

// Item — строка таблицы faq_items. Vec используется только при вставке (ReplaceAll);
// List/Get/Match его не выбирают.
type Item struct {
	ID        int64     `db:"id"`
	ChatID    int64     `db:"chat_id"`
	Question  string    `db:"question"`
	Answer    string    `db:"answer"`
	Vec       []float32 `db:"-"`
	UpdatedAt time.Time `db:"updated_at"`
}

// Repo читает/пишет faq_items.
type Repo struct {
	db *sqlx.DB
}

// NewRepo создаёт репозиторий.
func NewRepo(db *sqlx.DB) *Repo { return &Repo{db: db} }

// ReplaceAll атомарно заменяет все записи чата: в одной транзакции удаляет старые и
// вставляет новые. question_vec = NULL если it.Vec == nil.
func (r *Repo) ReplaceAll(ctx context.Context, chatID int64, items []Item) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM faq_items WHERE chat_id = $1`, chatID); err != nil {
		return fmt.Errorf("delete faq: %w", err)
	}

	for _, it := range items {
		var vecVal interface{}
		if it.Vec != nil {
			vecVal = pgvector.NewVector(it.Vec)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO faq_items (chat_id, question, answer, question_vec, updated_at)
			VALUES ($1, $2, $3, $4, now())
		`, chatID, it.Question, it.Answer, vecVal); err != nil {
			return fmt.Errorf("insert faq item: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit faq: %w", err)
	}
	return nil
}

// List возвращает все записи чата в порядке id.
func (r *Repo) List(ctx context.Context, chatID int64) ([]Item, error) {
	var rows []Item
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT id, chat_id, question, answer, updated_at
		FROM faq_items
		WHERE chat_id = $1
		ORDER BY id
	`, chatID); err != nil {
		return nil, fmt.Errorf("list faq: %w", err)
	}
	return rows, nil
}

// Get возвращает запись по id. (nil, nil) если не найдено.
func (r *Repo) Get(ctx context.Context, id int64) (*Item, error) {
	var it Item
	err := r.db.GetContext(ctx, &it, `
		SELECT id, chat_id, question, answer, updated_at
		FROM faq_items
		WHERE id = $1
	`, id)
	if err != nil {
		if errIsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get faq: %w", err)
	}
	return &it, nil
}

// UpdateAnswer правит ответ записи и обновляет updated_at.
func (r *Repo) UpdateAnswer(ctx context.Context, id int64, answer string) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE faq_items SET answer = $2, updated_at = now() WHERE id = $1
	`, id, answer); err != nil {
		return fmt.Errorf("update faq answer: %w", err)
	}
	return nil
}

// Match ищет записи чата, чей question_vec лежит в радиусе maxDist (косинусное
// расстояние <=>) к queryVec. Порядок — по возрастанию расстояния. limit<=0 → 1.
func (r *Repo) Match(ctx context.Context, chatID int64, queryVec []float32, maxDist float64, limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 1
	}
	var rows []Item
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT id, chat_id, question, answer, updated_at
		FROM faq_items
		WHERE chat_id = $2
		  AND question_vec IS NOT NULL
		  AND (question_vec <=> $1) < $3
		ORDER BY question_vec <=> $1
		LIMIT $4
	`, pgvector.NewVector(queryVec), chatID, maxDist, limit); err != nil {
		return nil, fmt.Errorf("match faq: %w", err)
	}
	return rows, nil
}

func errIsNoRows(err error) bool {
	return err == sql.ErrNoRows
}
