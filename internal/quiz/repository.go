// Package quiz — ежедневная викторина про чат и его участников.
package quiz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// Question — сгенерированный вопрос (до записи в БД).
type Question struct {
	Kind    string   // whoTop | whoFirst | stat | mentioned
	Prompt  string   // текст вопроса
	Opts    []string // 2 (Да/Нет) или 4 варианта
	Correct int      // индекс верной опции в Opts
}

// Row — строка quizzes.
type Row struct {
	ID        int64     `db:"id"`
	ChatID    int64     `db:"chat_id"`
	Kind      string    `db:"kind"`
	Question  string    `db:"question"`
	Opt1      string    `db:"opt1"`
	Opt2      string    `db:"opt2"`
	Opt3      string    `db:"opt3"`
	Opt4      string    `db:"opt4"`
	Correct   int       `db:"correct_opt"`
	MessageID int64     `db:"message_id"`
	CreatedAt time.Time `db:"created_at"`
}

// Opts возвращает варианты срезом.
func (r Row) Opts() []string {
	return []string{r.Opt1, r.Opt2, r.Opt3, r.Opt4}
}

// Repository читает/пишет викторины.
type Repository struct {
	db *sqlx.DB
}

// NewRepository создаёт repository.
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

// SaveQuiz записывает вопрос, возвращает id.
func (r *Repository) SaveQuiz(ctx context.Context, q *Question, chatID int64) (int64, error) {
	opts := [4]string{}
	for i, o := range q.Opts {
		if i < 4 {
			opts[i] = o
		}
	}
	var id int64
	if err := r.db.GetContext(ctx, &id, `
		INSERT INTO quizzes (chat_id, kind, question, opt1, opt2, opt3, opt4, correct_opt)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id
	`, chatID, q.Kind, q.Prompt, opts[0], opts[1], opts[2], opts[3], q.Correct); err != nil {
		return 0, fmt.Errorf("save quiz: %w", err)
	}
	return id, nil
}

// SetMessage привязывает id отправленного сообщения (для редакта live-tally).
func (r *Repository) SetMessage(ctx context.Context, id, messageID int64) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE quizzes SET message_id = $2 WHERE id = $1`, id, messageID); err != nil {
		return fmt.Errorf("set quiz message: %w", err)
	}
	return nil
}

// Get возвращает вопрос по id. Не найдено — (nil, nil).
func (r *Repository) Get(ctx context.Context, id int64) (*Row, error) {
	var row Row
	if err := r.db.GetContext(ctx, &row,
		`SELECT id, chat_id, kind, question, opt1, opt2, opt3, opt4, correct_opt, message_id, created_at
		 FROM quizzes WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get quiz: %w", err)
	}
	return &row, nil
}

// LastKind возвращает тип последней викторины в чате (чтобы не повторять два дня подряд).
// Нет викторин — ("", nil).
func (r *Repository) LastKind(ctx context.Context, chatID int64) (string, error) {
	var k string
	if err := r.db.GetContext(ctx, &k,
		`SELECT kind FROM quizzes WHERE chat_id = $1 ORDER BY id DESC LIMIT 1`, chatID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("last kind: %w", err)
	}
	return k, nil
}

// RecordBallot учитывает ответ участника. Возвращает new=true, если голос новый
// (ON CONFLICT DO NOTHING — один ответ на участника).
func (r *Repository) RecordBallot(ctx context.Context, quizID, userID int64, choice int) (bool, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO quiz_ballots (quiz_id, user_id, choice) VALUES ($1, $2, $3)
		ON CONFLICT (quiz_id, user_id) DO NOTHING
	`, quizID, userID, choice)
	if err != nil {
		return false, fmt.Errorf("record ballot: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountBallots возвращает общее число ответов и число верных.
func (r *Repository) CountBallots(ctx context.Context, quizID int64) (total, correct int, err error) {
	var row struct {
		Total   int `db:"total"`
		Correct int `db:"correct"`
	}
	if err = r.db.GetContext(ctx, &row, `
		SELECT count(*) AS total,
		       count(*) FILTER (WHERE b.choice = q.correct_opt) AS correct
		FROM quiz_ballots b
		JOIN quizzes q ON q.id = b.quiz_id
		WHERE b.quiz_id = $1
	`, quizID); err != nil {
		return 0, 0, fmt.Errorf("count ballots: %w", err)
	}
	return row.Total, row.Correct, nil
}

// BallotCounts возвращает распределение голосов по индексам опций (для раскрытия ответа).
func (r *Repository) BallotCounts(ctx context.Context, quizID int64) (map[int]int, error) {
	var rows []struct {
		Choice int `db:"choice"`
		Cnt    int `db:"cnt"`
	}
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT choice, count(*) AS cnt FROM quiz_ballots WHERE quiz_id = $1 GROUP BY choice`,
		quizID); err != nil {
		return nil, fmt.Errorf("ballot counts: %w", err)
	}
	m := make(map[int]int, len(rows))
	for _, rr := range rows {
		m[rr.Choice] = rr.Cnt
	}
	return m, nil
}
