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
	Kind        string   // категория темы (напр. «type-juggling», «psr»)
	Prompt      string   // текст вопроса
	Opts        []string // варианты (обычно 4)
	Correct     int      // индекс верной опции в Opts
	Explanation string   // почему верный ответ именно такой (показывается после ответа)
}

// Row — строка quizzes.
type Row struct {
	ID          int64     `db:"id"`
	ChatID      int64     `db:"chat_id"`
	Kind        string    `db:"kind"`
	Question    string    `db:"question"`
	Opt1        string    `db:"opt1"`
	Opt2        string    `db:"opt2"`
	Opt3        string    `db:"opt3"`
	Opt4        string    `db:"opt4"`
	Correct     int       `db:"correct_opt"`
	Explanation string    `db:"explanation"`
	MessageID   int64     `db:"message_id"`
	CreatedAt   time.Time `db:"created_at"`
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
		INSERT INTO quizzes (chat_id, kind, question, opt1, opt2, opt3, opt4, correct_opt, explanation)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id
	`, chatID, q.Kind, q.Prompt, opts[0], opts[1], opts[2], opts[3], q.Correct, q.Explanation); err != nil {
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
		`SELECT id, chat_id, kind, question, opt1, opt2, opt3, opt4, correct_opt, explanation, message_id, created_at
		 FROM quizzes WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get quiz: %w", err)
	}
	return &row, nil
}

// RecentKey — категория и текст недавнего вопроса (для дедупа по содержанию).
type RecentKey struct {
	Category string
	Question string
}

// RecentKeys возвращает категории и тексты вопросов чата за последние days дней (для
// дедупа: не повторять тему и не постить тот же вопрос).
func (r *Repository) RecentKeys(ctx context.Context, chatID int64, days int) ([]RecentKey, error) {
	var rows []RecentKey
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT kind AS category, question FROM quizzes
		WHERE chat_id = $1 AND created_at > now() - make_interval(days => $2)
		ORDER BY id DESC
	`, chatID, days); err != nil {
		return nil, fmt.Errorf("recent quiz keys: %w", err)
	}
	return rows, nil
}

// PostedToday — сколько викторин уже постилось в чате сегодня (по дате сервера БД).
func (r *Repository) PostedToday(ctx context.Context, chatID int64) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n, `
		SELECT count(*) FROM quizzes
		WHERE chat_id = $1 AND created_at >= date_trunc('day', now())
	`, chatID); err != nil {
		return 0, fmt.Errorf("posted today: %w", err)
	}
	return n, nil
}

// LastPostAt — время последнего квиза в чате; ok=false, если квизов ещё не было.
func (r *Repository) LastPostAt(ctx context.Context, chatID int64) (time.Time, bool, error) {
	var nt sql.NullTime
	if err := r.db.GetContext(ctx, &nt,
		`SELECT MAX(created_at) FROM quizzes WHERE chat_id = $1`, chatID); err != nil {
		return time.Time{}, false, fmt.Errorf("last post at: %w", err)
	}
	if !nt.Valid {
		return time.Time{}, false, nil
	}
	return nt.Time, true, nil
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
