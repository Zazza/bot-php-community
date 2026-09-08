package news

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// Repository — доступ к news_posted (дедуп новостей по нормализованному URL).
type Repository struct{ db *sqlx.DB }

// NewRepository создаёт repository.
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

// PostedHashes возвращает множество хэшей уже постившихся ссылок в чате.
func (r *Repository) PostedHashes(ctx context.Context, chatID int64) (map[string]struct{}, error) {
	var hashes []string
	if err := r.db.SelectContext(ctx, &hashes,
		`SELECT url_hash FROM news_posted WHERE chat_id = $1`, chatID); err != nil {
		return nil, fmt.Errorf("news posted hashes: %w", err)
	}
	out := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		out[h] = struct{}{}
	}
	return out, nil
}

// MarkPosted отмечает ссылки как постившиеся (идемпотентно).
func (r *Repository) MarkPosted(ctx context.Context, chatID int64, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	for _, h := range hashes {
		if _, err := r.db.ExecContext(ctx, `
			INSERT INTO news_posted (chat_id, url_hash) VALUES ($1, $2)
			ON CONFLICT (chat_id, url_hash) DO NOTHING
		`, chatID, h); err != nil {
			return fmt.Errorf("mark news posted: %w", err)
		}
	}
	return nil
}

// FakeRow — строка news_fake_posts (память пятничных выпусков).
type FakeRow struct {
	ID       int64     `db:"id"`
	Rubric   string    `db:"rubric"`
	Body     string    `db:"body"`
	PostedAt time.Time `db:"posted_at"`
}

// SaveFake сохраняет вышедший пятничный выпуск (анти-повтор тем следующих выпусков).
func (r *Repository) SaveFake(ctx context.Context, chatID int64, rubric, body string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO news_fake_posts (chat_id, rubric, body) VALUES ($1, $2, $3)`,
		chatID, rubric, body)
	if err != nil {
		return fmt.Errorf("save fake news: %w", err)
	}
	return nil
}

// ListFake возвращает последние limit пятничных выпусков чата (свежие первыми).
func (r *Repository) ListFake(ctx context.Context, chatID int64, limit int) ([]FakeRow, error) {
	var rows []FakeRow
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT id, rubric, body, posted_at
		FROM news_fake_posts WHERE chat_id = $1
		ORDER BY id DESC LIMIT $2
	`, chatID, limit); err != nil {
		return nil, fmt.Errorf("list fake news: %w", err)
	}
	return rows, nil
}

// ShownWallpapers возвращает множество ключей уже показанных обоев чата.
func (r *Repository) ShownWallpapers(ctx context.Context, chatID int64) (map[string]struct{}, error) {
	var keys []string
	if err := r.db.SelectContext(ctx, &keys,
		`SELECT wallpaper FROM news_wallpapers WHERE chat_id = $1`, chatID); err != nil {
		return nil, fmt.Errorf("wallpapers shown: %w", err)
	}
	out := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		out[k] = struct{}{}
	}
	return out, nil
}

// MarkWallpapersShown отмечает обои как показанные (одним batch-запросом, идемпотентно).
func (r *Repository) MarkWallpapersShown(ctx context.Context, chatID int64, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	values := make([]string, 0, len(keys))
	args := make([]interface{}, 0, len(keys)+1)
	args = append(args, chatID)
	for i, k := range keys {
		values = append(values, fmt.Sprintf("($1, $%d)", i+2))
		args = append(args, k)
	}
	q := `INSERT INTO news_wallpapers (chat_id, wallpaper) VALUES ` +
		strings.Join(values, ", ") + ` ON CONFLICT (chat_id, wallpaper) DO NOTHING`
	if _, err := r.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("mark wallpapers shown: %w", err)
	}
	return nil
}

// ResetWallpapers очищает историю показа обоев чата — цикл пула начинается заново.
func (r *Repository) ResetWallpapers(ctx context.Context, chatID int64) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM news_wallpapers WHERE chat_id = $1`, chatID); err != nil {
		return fmt.Errorf("reset wallpapers: %w", err)
	}
	return nil
}

// hashURL — стабильный хэш нормализованного URL для дедупа: нижний регистр хоста,
// без фрагмента и utm_* параметров, без висячего слэша в пути.
func hashURL(link string) string {
	raw := strings.TrimSpace(link)
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		u.Fragment = ""
		u.Host = strings.ToLower(u.Host)
		q := u.Query()
		for k := range q {
			if strings.HasPrefix(strings.ToLower(k), "utm_") {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode()
		u.Path = strings.TrimRight(u.Path, "/")
		raw = u.String()
	}
	sum := sha1.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}
