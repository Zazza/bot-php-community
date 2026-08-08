// Package news — ежедневный PHP-дайджест из внешних RSS/Atom-источников
// (официальные релизы/пакеты + топ обсуждаемого в публичных PHP-сообществах + блоги
// ключевых PHP-разработчиков): фетч фидов → дедуп по ссылке → LLM-куратория → пост в чат.
package news

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

// Source — RSS/Atom-источник новостей.
type Source struct {
	Name     string // человекочитаемое имя («Reddit r/PHP»)
	URL      string
	Category string // official | package | hub | blog
}

// DefaultSources — проверенные на проде фиды PHP-новостей. Блоги (category "blog") —
// персональные сайты ключевых PHP-разработчиков. Часть запрошенных источников сюда не
// вошла: php.announce покрыт php.net/releases, у ilia.ws фида нет вовсе, а exakat.io/feed/
// отдаёт тело настолько медленно, что стабильно таймаутит (>20с).
func DefaultSources() []Source {
	return []Source{
		{Name: "php.net (новости)", URL: "https://www.php.net/feed.atom", Category: "official"},
		{Name: "php.net (релизы)", URL: "https://www.php.net/releases/feed.php", Category: "official"},
		{Name: "Packagist (новые пакеты)", URL: "https://packagist.org/feeds/packages.rss", Category: "package"},
		{Name: "Packagist (релизы)", URL: "https://packagist.org/feeds/releases.rss", Category: "package"},
		{Name: "Reddit r/PHP", URL: "https://www.reddit.com/r/php/.rss", Category: "hub"},
		{Name: "Habr: PHP", URL: "https://habr.com/ru/rss/hub/php/", Category: "hub"},
		{Name: "Freek Van der Herten", URL: "https://freek.dev/feed", Category: "blog"},
		{Name: "James Titcumb", URL: "https://www.jamestitcumb.com/feed", Category: "blog"},
		{Name: "Ocramius", URL: "https://ocramius.github.io/atom.xml", Category: "blog"},
		{Name: "PHP-Тесто (блог)", URL: "https://php-testo.github.io/ru/feed.xml", Category: "blog"},
		{Name: "Tideways (блог)", URL: "https://tideways.com/feed/atom", Category: "blog"},
		{Name: "Jordi Boggiano (seld.be)", URL: "https://seld.be/feed.atom", Category: "blog"},
	}
}

// Item — элемент новости из фида.
type Item struct {
	Source    Source
	Title     string
	Link      string
	Published time.Time
}

const (
	newsUA       = "php-bot/1.0 (+https://github.com/Zazza/bot-php-community)"
	feedTimeout  = 20 * time.Second
	maxFeedBytes = 5 << 20
)

// Fetch тащит все источники, устойчиво к отказам отдельных фидов (упавший — warn + skip).
// Возвращает собранные элементы (пусто, если все фиды упали —caller решит, что дальше).
func Fetch(ctx context.Context, sources []Source) []Item {
	parser := gofeed.NewParser()
	client := &http.Client{Timeout: feedTimeout}
	var items []Item
	for _, src := range sources {
		its, err := fetchOne(ctx, client, parser, src)
		if err != nil {
			slog.Warn("news feed fetch", "src", src.Name, "err", err)
			continue
		}
		items = append(items, its...)
	}
	return items
}

// urlAttrRe — URL-несущие атрибуты (href/url). Применяется только при ошибке парсинга:
// триммит пробелы в значениях, чтобы починить фиды с битым href вроде " https://...".
var urlAttrRe = regexp.MustCompile(`(?i)\b(href|url)\s*=\s*"[^"]*"`)

func sanitizeURLAttrs(raw string) string {
	return urlAttrRe.ReplaceAllStringFunc(raw, func(m string) string {
		qi := strings.IndexByte(m, '"')
		if qi < 0 || qi == len(m)-1 {
			return m
		}
		return m[:qi+1] + strings.TrimSpace(m[qi+1:len(m)-1]) + `"`
	})
}

func fetchOne(ctx context.Context, client *http.Client, parser *gofeed.Parser, src Source) ([]Item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", newsUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return nil, err
	}
	feed, err := parser.ParseString(string(body))
	if err != nil {
		// Часть фидов (напр. php.net) содержит URL-атрибуты с лидирующим пробелом
		// (" https://..."), на котором url-резолвер gofeed'а падает и роняет ВЕСЬ фид.
		// Тримим значения href/url и парсим повторно; не помогло — отдаём исходную ошибку.
		feed, err = parser.ParseString(sanitizeURLAttrs(string(body)))
		if err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
		slog.Info("news feed recovered after URL sanitize", "src", src.Name)
	}
	out := make([]Item, 0, len(feed.Items))
	for _, it := range feed.Items {
		link := it.Link
		if link == "" && len(it.Links) > 0 {
			link = it.Links[0]
		}
		if it.Title == "" || link == "" {
			continue
		}
		var ts time.Time
		if it.PublishedParsed != nil {
			ts = *it.PublishedParsed
		} else if it.UpdatedParsed != nil {
			ts = *it.UpdatedParsed
		}
		out = append(out, Item{Source: src, Title: it.Title, Link: link, Published: ts})
	}
	return out, nil
}
