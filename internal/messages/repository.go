// Package messages — repository: сохранение сообщений чата и поиск по векторам.
package messages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// Message — строка таблицы messages.
type Message struct {
	ID        int64     `db:"id"`
	ChatID    int64     `db:"chat_id"`
	UserID    int64     `db:"user_id"`
	Username  string    `db:"username"`
	Text      string    `db:"text"`
	TS        time.Time `db:"ts"`
	ReplyToID *int64    `db:"reply_to_id"`
}

// Repository читает/пишет сообщения и embeddings.
type Repository struct {
	db *sqlx.DB
}

// New создаёт repository.
func New(db *sqlx.DB) *Repository { return &Repository{db: db} }

// Save сохраняет сообщение (идемпотентно по PK — ON CONFLICT ничего не делает).
func (r *Repository) Save(ctx context.Context, m *Message) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO messages (id, chat_id, user_id, username, text, ts, reply_to_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO NOTHING
	`, m.ID, m.ChatID, m.UserID, m.Username, m.Text, m.TS, m.ReplyToID)
	if err != nil {
		return fmt.Errorf("save message: %w", err)
	}
	return nil
}

// Last возвращает последние N сообщений чата в хронологическом порядке (старые→новые).
func (r *Repository) Last(ctx context.Context, chatID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 15
	}
	var rows []Message
	// Берём последние N (DESC) и реверсим в коде для хронологического порядка.
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT id, chat_id, user_id, username, text, ts, reply_to_id
		FROM messages
		WHERE chat_id = $1
		ORDER BY ts DESC
		LIMIT $2
	`, chatID, limit); err != nil {
		return nil, fmt.Errorf("last messages: %w", err)
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// NextAfter возвращает первое сообщение чата с ts > after (хронологически следующее
// за past.TS — вероятный ответ/реакция). Если таких строк нет — (nil, nil) без ошибки.
func (r *Repository) NextAfter(ctx context.Context, chatID int64, after time.Time) (*Message, error) {
	var m Message
	err := r.db.GetContext(ctx, &m, `
		SELECT id, chat_id, user_id, username, text, ts, reply_to_id
		FROM messages
		WHERE chat_id = $1 AND ts > $2
		ORDER BY ts ASC
		LIMIT 1
	`, chatID, after)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("next after: %w", err)
	}
	return &m, nil
}

// CountSince возвращает число сообщений в чате за период [since, now].
func (r *Repository) CountSince(ctx context.Context, chatID int64, since time.Time) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n, `
		SELECT count(*) FROM messages WHERE chat_id = $1 AND ts >= $2
	`, chatID, since); err != nil {
		return 0, fmt.Errorf("count since: %w", err)
	}
	return n, nil
}

// CountBetween возвращает число сообщений чата за период [from, to).
func (r *Repository) CountBetween(ctx context.Context, chatID int64, from, to time.Time) (int, error) {
	var n int
	if err := r.db.GetContext(ctx, &n, `
		SELECT count(*) FROM messages WHERE chat_id = $1 AND ts >= $2 AND ts < $3
	`, chatID, from, to); err != nil {
		return 0, fmt.Errorf("count between: %w", err)
	}
	return n, nil
}

// CountByUser возвращает число сообщений участника в чате (не больше limit — capped-count,
// чтобы для ветерана с тысячами сообщений скан ограничивался limit строками). limit<=0 — без
// лимита. Источник at-risk-порога анти-спама: мало сообщений → полная LLM-классификация.
func (r *Repository) CountByUser(ctx context.Context, chatID, userID int64, limit int) (int, error) {
	var n int
	if limit > 0 {
		if err := r.db.GetContext(ctx, &n, `
			SELECT count(*) FROM (SELECT 1 FROM messages WHERE chat_id = $1 AND user_id = $2 LIMIT $3) t
		`, chatID, userID, limit); err != nil {
			return 0, fmt.Errorf("count by user: %w", err)
		}
		return n, nil
	}
	if err := r.db.GetContext(ctx, &n, `
		SELECT count(*) FROM messages WHERE chat_id = $1 AND user_id = $2
	`, chatID, userID); err != nil {
		return 0, fmt.Errorf("count by user: %w", err)
	}
	return n, nil
}

// Since возвращает все сообщения чата за период (хронологически).
func (r *Repository) Since(ctx context.Context, chatID int64, from, to time.Time) ([]Message, error) {
	var rows []Message
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT id, chat_id, user_id, username, text, ts, reply_to_id
		FROM messages
		WHERE chat_id = $1 AND ts >= $2 AND ts < $3
		ORDER BY ts ASC
	`, chatID, from, to); err != nil {
		return nil, fmt.Errorf("messages since: %w", err)
	}
	return rows, nil
}

// LastByUser возвращает последние N сообщений пользователя (для ручной judge-проверки).
func (r *Repository) LastByUser(ctx context.Context, userID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 10
	}
	var rows []Message
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT id, chat_id, user_id, username, text, ts, reply_to_id
		FROM messages
		WHERE user_id = $1
		ORDER BY ts DESC
		LIMIT $2
	`, userID, limit); err != nil {
		return nil, fmt.Errorf("last by user: %w", err)
	}
	return rows, nil
}

// FirstByUser возвращает ПЕРВЫЕ N сообщений пользователя (хронологически с начала).
func (r *Repository) FirstByUser(ctx context.Context, userID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 10
	}
	var rows []Message
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT id, chat_id, user_id, username, text, ts, reply_to_id
		FROM messages
		WHERE user_id = $1
		ORDER BY ts ASC
		LIMIT $2
	`, userID, limit); err != nil {
		return nil, fmt.Errorf("first by user: %w", err)
	}
	return rows, nil
}

// ByUsername возвращает последние сообщения пользователя в чате по @username за период.
// Порядок DESC (свежие первыми). since/until == nil — без границы.
func (r *Repository) ByUsername(ctx context.Context, chatID int64, username string, since, until *time.Time, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 150
	}
	var rows []Message
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT id, chat_id, user_id, username, text, ts, reply_to_id
		FROM messages
		WHERE chat_id = $1 AND username = $2
		  AND ($3::timestamptz IS NULL OR ts >= $3)
		  AND ($4::timestamptz IS NULL OR ts < $4)
		ORDER BY ts DESC
		LIMIT $5
	`, chatID, username, since, until, limit); err != nil {
		return nil, fmt.Errorf("messages by username: %w", err)
	}
	return rows, nil
}

// ExpertByKeyword считает сообщения каждого автора, где упоминается тема (keyword,
// case-insensitive) — кто реально пишет по теме. В отличие от семантического поиска с
// жёстким косинус-порогом, даёт точный счёт упоминаний (напр. 85 по «yii3», а не «3»).
// Имя: users.username (настоящий @handle), fallback messages.username. user_id=0
// (без атрибуции) исключается.
func (r *Repository) ExpertByKeyword(ctx context.Context, chatID int64, topic string, limit int) ([]Expert, error) {
	normalized := normalizeTopic(topic)
	if len(normalized) < 2 {
		return nil, nil // слишком коротко/общо — не ищем (иначе паттерн '%%' совпадёт со всем)
	}
	if limit <= 0 {
		limit = 5
	}
	pattern := "%" + normalized + "%"
	var rows []Expert
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT m.user_id,
		       COALESCE(NULLIF(MAX(u.username),''), NULLIF(MAX(m.username),''), 'user') AS username,
		       count(*) AS cnt
		FROM messages m
		LEFT JOIN users u ON u.tg_user_id = m.user_id
		WHERE m.chat_id = $1 AND m.user_id <> 0
		  AND regexp_replace(lower(m.text), '[^0-9a-zа-яё]', '', 'g') ILIKE $2
		GROUP BY m.user_id
		ORDER BY cnt DESC
		LIMIT $3
	`, chatID, pattern, limit); err != nil {
		return nil, fmt.Errorf("experts keyword: %w", err)
	}
	return rows, nil
}

// normalizeTopic приводит тему к каноничному виду для keyword-поиска: нижний регистр,
// только буквы/цифры (латиница + кириллица), без разделителей. Так «yii3», «yii 3»,
// «Yii-3» совпадают. Должно соответствовать regexp_replace(...) в SQL ExpertByKeyword.
func normalizeTopic(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r >= 'а' && r <= 'я', r == 'ё':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// LastByUsername находит id пользователя и текст его последнего сообщения по @username.
// Возвращает (0, "", nil) если совпадений нет.
func (r *Repository) LastByUsername(ctx context.Context, username string) (userID int64, text string, err error) {
	var row struct {
		UserID int64  `db:"user_id"`
		Text   string `db:"text"`
	}
	if err := r.db.GetContext(ctx, &row, `
		SELECT user_id, text FROM messages
		WHERE username = $1
		ORDER BY ts DESC LIMIT 1
	`, username); err != nil {
		return 0, "", nil // не найдено → не падаем
	}
	return row.UserID, row.Text, nil
}

// Stats — базовая статистика чата.
type Stats struct {
	TotalMessages int      `db:"total_messages" json:"total_messages"`
	ActiveUsers   int      `db:"active_users" json:"active_users"`
	LastDay       int      `db:"last_day" json:"last_day"`
	TopPosters    []Poster `json:"top_posters"`
}

// Poster — активный участник.
type Poster struct {
	Username string `db:"username" json:"username"`
	Count    int    `db:"cnt" json:"count"`
}

// Stats считает сводку по чату за период. since/until == nil — без границы.
// last_day всегда «за последние 24ч» (свежее активности), остальные агрегаты фильтруются периодом.
func (r *Repository) Stats(ctx context.Context, chatID int64, since, until *time.Time) (*Stats, error) {
	s := &Stats{}
	if err := r.db.GetContext(ctx, s, `
		SELECT
			count(*) AS total_messages,
			count(DISTINCT user_id) AS active_users,
			count(*) FILTER (WHERE ts >= now() - interval '24 hours') AS last_day
		FROM messages
		WHERE chat_id = $1
		  AND ($2::timestamptz IS NULL OR ts >= $2)
		  AND ($3::timestamptz IS NULL OR ts < $3)
	`, chatID, since, until); err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	var top []Poster
	if err := r.db.SelectContext(ctx, &top, `
		SELECT COALESCE(NULLIF(username,''),'user') AS username, count(*) AS cnt
		FROM messages
		WHERE chat_id = $1
		  AND ($2::timestamptz IS NULL OR ts >= $2)
		  AND ($3::timestamptz IS NULL OR ts < $3)
		GROUP BY username
		ORDER BY cnt DESC
		LIMIT 5
	`, chatID, since, until); err != nil {
		return nil, fmt.Errorf("stats top: %w", err)
	}
	s.TopPosters = top
	return s, nil
}

// UserStats — поведенческая сводка участника (для /я).
type UserStats struct {
	Count    int
	CodeMsgs int
	AvgLen   float64
	PeakHour int // 0..23, -1 если сообщений нет
	FirstTS  time.Time
}

// UserStats считает по участнику: число сообщений, среднюю длину, число «кодовых» сообщений,
// пик активности по часу и дату первого сообщения. Колонка user_id (не tg_user_id).
func (r *Repository) UserStats(ctx context.Context, chatID, userID int64) (*UserStats, error) {
	s := &UserStats{PeakHour: -1}
	var row struct {
		Count    int       `db:"cnt"`
		CodeMsgs int       `db:"code_msgs"`
		AvgLen   float64   `db:"avg_len"`
		FirstTS  time.Time `db:"first_ts"`
	}
	if err := r.db.GetContext(ctx, &row, `
		SELECT count(*) AS cnt,
		       count(*) FILTER (WHERE text ILIKE '%'||repeat(chr(96),3)||'%' OR text LIKE '%=>%' OR text LIKE '%;') AS code_msgs,
		       COALESCE(avg(char_length(text)), 0) AS avg_len,
		       COALESCE(min(ts), now()) AS first_ts
		FROM messages WHERE chat_id = $1 AND user_id = $2
	`, chatID, userID); err != nil {
		return nil, fmt.Errorf("user stats: %w", err)
	}
	s.Count, s.CodeMsgs, s.AvgLen, s.FirstTS = row.Count, row.CodeMsgs, row.AvgLen, row.FirstTS
	if s.Count > 0 {
		var ph struct {
			H int `db:"h"`
		}
		if err := r.db.GetContext(ctx, &ph, `
			SELECT EXTRACT(HOUR FROM ts)::int AS h
			FROM messages WHERE chat_id = $1 AND user_id = $2
			GROUP BY h ORDER BY count(*) DESC, h ASC LIMIT 1
		`, chatID, userID); err == nil {
			s.PeakHour = ph.H
		}
	}
	return s, nil
}

// Anniversary — участник и дата его первого сообщения в чате.
type Anniversary struct {
	UserID   int64     `db:"user_id"`
	Username string    `db:"username"`
	First    time.Time `db:"first"`
}

// Anniversaries возвращает участников, у кого сегодня годовщина ПЕРВОГО сообщения в чате
// (по MIN(ts) сообщений — реальный заход, а не users.first_seen, который фиксирует лишь
// момент, когда бот начал отслеживать юзера). Месяц/день совпадают, год — не текущий.
func (r *Repository) Anniversaries(ctx context.Context, chatID int64, now time.Time) ([]Anniversary, error) {
	var rows []Anniversary
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT m.user_id,
		       COALESCE(NULLIF(MAX(u.username),''), NULLIF(MAX(m.username),''), 'user') AS username,
		       MIN(m.ts) AS first
		FROM messages m
		LEFT JOIN users u ON u.tg_user_id = m.user_id
		WHERE m.chat_id = $1 AND m.user_id <> 0
		GROUP BY m.user_id
		HAVING EXTRACT(MONTH FROM MIN(m.ts)) = EXTRACT(MONTH FROM $2::date)
		   AND EXTRACT(DAY   FROM MIN(m.ts)) = EXTRACT(DAY   FROM $2::date)
		   AND EXTRACT(YEAR  FROM MIN(m.ts)) <> EXTRACT(YEAR FROM $2::date)
	`, chatID, now); err != nil {
		return nil, fmt.Errorf("anniversaries: %w", err)
	}
	return rows, nil
}

// FormatContext собирает сообщения в строку для подачи в системный промпт LLM.
func FormatContext(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		who := m.Username
		if who == "" {
			who = "user"
		}
		fmt.Fprintf(&b, "[%s] %s: %s\n", m.TS.Format("15:04:05"), who, m.Text)
	}
	return b.String()
}
