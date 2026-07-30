// Package websearch — клиент SearXNG (JSON API) для актуализации ответов бота
// свежими веб-данными: версии, релизы, даты, новости — то, чего нет в истории чата
// и что модель знает по устаревшей обучающей выборке.
package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Result — один найденный веб-результат.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"` // сниппет
}

// Searcher ходит в SearXNG /search?format=json.
type Searcher struct {
	baseURL    string
	maxResults int
	client     *http.Client
}

// New создаёт Searcher. maxResults<=0 → 5.
func New(baseURL string, maxResults int) *Searcher {
	if maxResults <= 0 {
		maxResults = 5
	}
	return &Searcher{
		baseURL:    strings.TrimRight(baseURL, "/"),
		maxResults: maxResults,
		client:     &http.Client{Timeout: 15 * time.Second},
	}
}

type searxResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Search возвращает top-N результатов по запросу.
func (s *Searcher) Search(ctx context.Context, query string) ([]Result, error) {
	if s.baseURL == "" {
		return nil, errors.New("searxng base url is empty")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errors.New("empty query")
	}

	u := s.baseURL + "/search?format=json&q=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		safe := string(b)
		if len(safe) > 200 {
			safe = safe[:200] + "..."
		}
		return nil, fmt.Errorf("searxng HTTP %d: %s", resp.StatusCode, safe)
	}

	var r searxResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode searxng: %w", err)
	}

	out := make([]Result, 0, s.maxResults)
	for _, res := range r.Results {
		if res.URL == "" {
			continue
		}
		out = append(out, Result{Title: res.Title, URL: res.URL, Content: res.Content})
		if len(out) >= s.maxResults {
			break
		}
	}
	return out, nil
}

// FormatResults собирает результаты в блок для системного промпта LLM.
func FormatResults(rs []Result) string {
	var b strings.Builder
	for i, r := range rs {
		title := strings.ReplaceAll(strings.TrimSpace(r.Title), "\n", " ")
		snip := strings.ReplaceAll(strings.TrimSpace(r.Content), "\n", " ")
		if snip == "" {
			fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, title, r.URL)
			continue
		}
		if len(snip) > 300 {
			snip = snip[:300] + "…"
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, title, r.URL, snip)
	}
	return b.String()
}
