package news

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"phpbot/internal/llm"
	"phpbot/internal/prompts"
)

// Poster — минимальный интерфейс постинга в чат (реализует tg.PosterImpl).
type Poster interface {
	PostMessage(ctx context.Context, chatID int64, text string) error
}

// ErrNoNews — нет свежих непостившихся новостей. Cron трактует как не-ошибку (debug),
// ручная команда /news — сообщает пользователю.
var ErrNoNews = errors.New("no fresh news")

// freshWindow — считаем свежими новости за этот период.
const freshWindow = 7 * 24 * time.Hour

// Digester собирает PHP-дайджест: фетч фидов → дедуп → LLM-куратория → пост.
type Digester struct {
	sources []Source
	llm     *llm.LLMClient
	repo    *Repository
	api     Poster
	chatIDs []int64
	cron    *cron.Cron
}

// NewDigester создаёт Digester. sources=nil → DefaultSources.
func NewDigester(llm *llm.LLMClient, repo *Repository, api Poster, chatIDs []int64, sources []Source) *Digester {
	if len(sources) == 0 {
		sources = DefaultSources()
	}
	return &Digester{sources: sources, llm: llm, repo: repo, api: api, chatIDs: chatIDs}
}

// Start регистрирует пост дайджеста (по умолчанию ежедневно 19:00) и запускает cron.
func (d *Digester) Start(ctx context.Context, spec string) error {
	c := cron.New()
	_, err := c.AddFunc(spec, func() {
		bg, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for _, chatID := range d.chatIDs {
			if err := d.Post(bg, chatID); err != nil {
				if errors.Is(err, ErrNoNews) {
					slog.Debug("news digest skipped", "chat_id", chatID, "err", err)
				} else {
					slog.Error("news digest", "chat_id", chatID, "err", err)
				}
			}
		}
	})
	if err != nil {
		return fmt.Errorf("add news cron: %w", err)
	}
	c.Start()
	d.cron = c
	slog.Info("news scheduler started", "cron", spec, "sources", len(d.sources))
	return nil
}

// Stop останавливает cron.
func (d *Digester) Stop() {
	if d.cron != nil {
		d.cron.Stop()
	}
}

// Post собирает и постит дайджест свежих PHP-новостей в чат.
func (d *Digester) Post(ctx context.Context, chatID int64) error {
	items := Fetch(ctx, d.sources)
	fresh := filterFresh(items, time.Now().Add(-freshWindow))
	posted, err := d.repo.PostedHashes(ctx, chatID)
	if err != nil {
		return err
	}
	cands := dropPosted(fresh, posted)
	if len(cands) == 0 {
		return ErrNoNews
	}
	resp, _, _, err := d.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompts.Get(prompts.News)},
		{Role: "user", Content: formatCandidates(cands)},
	})
	if err != nil {
		return fmt.Errorf("news llm: %w", err)
	}
	body := strings.TrimSpace(resp)
	if body == "" || strings.EqualFold(body, "SKIP") {
		return ErrNoNews
	}
	composed := composeDigest(body, cands)
	if composed == "" {
		return ErrNoNews
	}
	text := "📰 **PHP-дайджест**\n\n" + composed
	if err := d.api.PostMessage(ctx, chatID, text); err != nil {
		return fmt.Errorf("post news: %w", err)
	}
	hashes := make([]string, 0, len(cands))
	for _, c := range cands {
		hashes = append(hashes, hashURL(c.Link))
	}
	if err := d.repo.MarkPosted(ctx, chatID, hashes); err != nil {
		slog.Warn("mark news posted", "err", err)
	}
	return nil
}

// filterFresh оставляет новости не старше cutoff (без даты — оставляем).
func filterFresh(items []Item, cutoff time.Time) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if it.Published.IsZero() || !it.Published.Before(cutoff) {
			out = append(out, it)
		}
	}
	return out
}

// dropPosted убирает уже постившиеся (по хэшу) и дубликаты ссылок внутри пачки.
func dropPosted(items []Item, posted map[string]struct{}) []Item {
	out := make([]Item, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		h := hashURL(it.Link)
		if _, p := posted[h]; p {
			continue
		}
		if _, s := seen[h]; s {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, it)
	}
	return out
}

// formatCandidates — нумерованный список свежих новостей для подачи в LLM.
func formatCandidates(items []Item) string {
	var b strings.Builder
	now := time.Now()
	for i, it := range items {
		age := ""
		if !it.Published.IsZero() {
			d := int(now.Sub(it.Published).Hours() / 24)
			if d < 0 {
				d = 0
			}
			age = fmt.Sprintf(" (%dд назад)", d)
		}
		fmt.Fprintf(&b, "%d. [%s] %s%s — %s\n", i+1, it.Source.Name, it.Title, age, it.Link)
	}
	return b.String()
}

// composeDigest парсит ответ LLM: по ведущему номеру строки находит свой кандидат и
// подставляет ЕГО ссылку (LLM ссылки не пишет) как markdown-ссылку [🔗](url) — mdToHTML
// превратит её в <a href>, и длинный URL будет скрыт за кликабельным 🔗. Пункты разделены
// пустой строкой. Строки без распознанного номера/вне диапазона пропускаются. Пусто → "".
func composeDigest(body string, cands []Item) string {
	var blocks []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx, ok := parseLeadingIndex(line)
		if !ok || idx < 1 || idx > len(cands) {
			continue
		}
		rest := strings.TrimLeft(line, "0123456789")
		rest = strings.TrimSpace(strings.TrimLeft(rest, ".)"))
		if rest == "" {
			continue
		}
		blocks = append(blocks, rest+" [🔗]("+cands[idx-1].Link+")")
	}
	return strings.Join(blocks, "\n\n")
}

// parseLeadingIndex читает ведущее целое число в строке.
func parseLeadingIndex(s string) (int, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
