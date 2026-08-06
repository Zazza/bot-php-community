package news

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

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
