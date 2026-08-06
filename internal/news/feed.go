// Package news — еженедельный PHP-дайджест из внешних RSS/Atom-источников
// (официальные релизы/пакеты + топ обсуждаемого в публичных PHP-сообществах):
// фетч фидов → дедуп по ссылке → LLM-куратория с русскими описаниями → пост в чат.
package news

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mmcdole/gofeed"
)

// Source — RSS/Atom-источник новостей.
type Source struct {
	Name     string // человекочитаемое имя («Reddit r/PHP»)
	URL      string
	Category string // official | package | hub
}

// DefaultSources — проверенные на проде фиды PHP-новостей.
func DefaultSources() []Source {
	return []Source{
		{Name: "php.net (новости)", URL: "https://www.php.net/feed.atom", Category: "official"},
		{Name: "php.net (релизы)", URL: "https://www.php.net/releases/feed.php", Category: "official"},
		{Name: "Packagist (новые пакеты)", URL: "https://packagist.org/feeds/packages.rss", Category: "package"},
		{Name: "Packagist (релизы)", URL: "https://packagist.org/feeds/releases.rss", Category: "package"},
		{Name: "Reddit r/PHP", URL: "https://www.reddit.com/r/php/.rss", Category: "hub"},
		{Name: "Habr: PHP", URL: "https://habr.com/ru/rss/hub/php/", Category: "hub"},
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
		return nil, fmt.Errorf("parse: %w", err)
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
