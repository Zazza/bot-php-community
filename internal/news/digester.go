package news

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
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

// freshWindow — считаем свежими новости за этот период. Ежедневный вечерний дайджест:
// 48ч держат фокус на свежем + переживают один пропущенный прогон (бот лежал сутки),
// не захватывая «совсем старые».
const freshWindow = 48 * time.Hour

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

// Post собирает и постит дайджест свежих PHP-новостей: статьи и пакеты отдельными
// LLM-секциями, pre-release версии отбрасываются.
func (d *Digester) Post(ctx context.Context, chatID int64) error {
	items := Fetch(ctx, d.sources)
	fresh := filterFresh(items, time.Now().Add(-freshWindow))
	posted, err := d.repo.PostedHashes(ctx, chatID)
	if err != nil {
		return err
	}
	cands := dropPosted(fresh, posted)
	cands = dropPreReleases(cands)
	articles, packages := splitByBucket(cands)
	if len(articles) == 0 && len(packages) == 0 {
		return ErrNoNews
	}
	artText, artUsed := d.composeArticlesFromLLM(ctx, articles)
	pkgText, pkgUsed := d.composePackagesFromLLM(ctx, packages)
	if artText == "" && pkgText == "" {
		return ErrNoNews
	}
	text := assembleDigest(artText, pkgText)
	if err := d.api.PostMessage(ctx, chatID, text); err != nil {
		return fmt.Errorf("post news: %w", err)
	}
	used := make([]string, 0, len(artUsed)+len(pkgUsed))
	used = append(used, artUsed...)
	used = append(used, pkgUsed...)
	if err := d.repo.MarkPosted(ctx, chatID, hashesOf(used)); err != nil {
		slog.Warn("mark news posted", "err", err)
	}
	return nil
}

// composeArticlesFromLLM делает LLM-вызов секции статей; пустой бакет, SKIP или ошибка → ("",nil)
// (fail-safe: ронять весь пост из-за одной секции не стоит — вторая постируется отдельно).
func (d *Digester) composeArticlesFromLLM(ctx context.Context, articles []Item) (string, []string) {
	if len(articles) == 0 {
		return "", nil
	}
	articles = sortByArticlesPriority(articles)
	resp, _, _, err := d.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompts.Get(prompts.News)},
		{Role: "user", Content: formatCandidates(articles)},
	})
	if err != nil {
		slog.Warn("news articles llm", "err", err)
		return "", nil
	}
	body := strings.TrimSpace(resp)
	if body == "" || strings.EqualFold(body, "SKIP") {
		return "", nil
	}
	return composeArticles(body, articles)
}

// composePackagesFromLLM делает LLM-вызов секции пакетов; пустой бакет, SKIP или ошибка → ("",nil).
func (d *Digester) composePackagesFromLLM(ctx context.Context, packages []Item) (string, []string) {
	if len(packages) == 0 {
		return "", nil
	}
	resp, _, _, err := d.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompts.Get(prompts.Packages)},
		{Role: "user", Content: formatCandidates(packages)},
	})
	if err != nil {
		slog.Warn("news packages llm", "err", err)
		return "", nil
	}
	body := strings.TrimSpace(resp)
	if body == "" || strings.EqualFold(body, "SKIP") {
		return "", nil
	}
	return composePackages(body, packages)
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

// dropPreReleases убирает элементы с pre-release версией в Title (alpha/beta/rc/dev/snapshot/pl).
func dropPreReleases(items []Item) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if preReleaseRe.MatchString(it.Title) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// splitByBucket делит кандидатов: articles={official,hub,blog}, packages={package}.
// Неизвестная категория → articles (fail-safe: лучше показать, чем потерять).
func splitByBucket(items []Item) (articles, packages []Item) {
	for _, it := range items {
		switch it.Source.Category {
		case "package":
			packages = append(packages, it)
		default:
			articles = append(articles, it)
		}
	}
	return articles, packages
}

// sortByArticlesPriority сортирует articles: blog → official → hub; внутри категории
// сохраняет исходный порядок (стабильно). Возвращает новый срез (не мутирует вход).
func sortByArticlesPriority(items []Item) []Item {
	const (
		blog = iota
		official
		hub
		other
	)
	rank := func(cat string) int {
		switch cat {
		case "blog":
			return blog
		case "official":
			return official
		case "hub":
			return hub
		default:
			return other
		}
	}
	out := make([]Item, len(items))
	copy(out, items)
	sort.SliceStable(out, func(i, j int) bool {
		return rank(out[i].Source.Category) < rank(out[j].Source.Category)
	})
	return out
}

// formatCandidates — нумерованный список свежих материалов с тегом категории источника
// для подачи в LLM. Строка: "N. [category] Source Name — Title (Xд назад) — link".
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
		fmt.Fprintf(&b, "%d. [%s] %s — %s%s — %s\n", i+1, it.Source.Category, it.Source.Name, it.Title, age, it.Link)
	}
	return b.String()
}

// hashesOf возвращает хэши переданных ссылок.
func hashesOf(links []string) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, hashURL(l))
	}
	return out
}

// urlTokenRe — ссылка в строке ответа LLM.
var urlTokenRe = regexp.MustCompile(`https?://\S+`)

// leadingNumRe — ведущая нумерация "N. " / "N) " (контракт — не нумеровать, но защитимся:
// при перенумерации номер всё равно не используется для привязки ссылки). \s+ (не \s*):
// требует пробел после маркера, иначе калечит заголовок-версию без ** вроде "8.4 Release".
var leadingNumRe = regexp.MustCompile(`^\s*\d+[.)]\s+`)

// preReleaseRe — детект pre-release версии в Title. Цифра перед маркером обязательна
// (0 false-positive на «archive», «transaction», «models-dev», «deploy»).
var preReleaseRe = regexp.MustCompile(`(?i)\d[-._]?(alpha|beta|rc|dev|snapshot|pl)\d*`)

// articleLineRe — разделитель заголовок/описание секции статей: ведущее **...**, затем описание.
var articleLineRe = regexp.MustCompile(`(?s)^\*\*(.+?)\*\*\s*[—–-]?\s*(.*)$`)

// packageSepRe — первое " — "/" – " (em/en/hyphen с пробелами) — разделитель name/description пакета.
var packageSepRe = regexp.MustCompile(`\s+[—–-]\s+`)

// mdBrackets убирает квадратные скобки из LLM-контролируемого текста (title/name/desc), чтобы он
// не разорвал markdown-обёртку [..](..) (напр. утянутый тег [blog]) и не собрал ссылку с произвольной
// схемой ([x](javascript:...)). Явная защита, не опирается на побочный эффект urlTokenRe.
var mdBrackets = strings.NewReplacer("[", "", "]", "")

// stripMD удаляет markdown-значимые квадратные скобки из текста ссылки/описания.
func stripMD(s string) string { return mdBrackets.Replace(s) }

// composeArticles парсит ответ LLM-секции статей. Контракт строки:
//
//	**Заголовок** — описание <ССЫЛКА>
//
// Достаёт URL (matchAllowedURL против кандидатов); отбрасывает строку, если URL не из
// кандидатов (защита от подмены). Убирает url-токены/нумерацию, разделяет заголовок/описание.
// Блок "[Заголовок](url)\nОписание" (описание опц.). Блоки через "\n\n". Возвращает (text, used),
// где used — слайс РАВНЫХ url (не хэшей), реально вошедших в пост.
func composeArticles(body string, cands []Item) (string, []string) {
	allowed := make(map[string]string, len(cands)) // hashURL(link) → link
	for _, c := range cands {
		allowed[hashURL(c.Link)] = c.Link
	}
	var blocks []string
	var used []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		raw := matchAllowedURL(line, allowed)
		if raw == "" {
			continue
		}
		clean := urlTokenRe.ReplaceAllString(line, "")
		clean = strings.TrimSpace(clean)
		clean = strings.TrimSpace(strings.TrimRight(clean, " —–-"))
		clean = leadingNumRe.ReplaceAllString(clean, "")
		clean = strings.TrimSpace(clean)
		if clean == "" {
			continue
		}
		var title, desc string
		if m := articleLineRe.FindStringSubmatch(clean); m != nil {
			title = stripMD(strings.TrimSpace(m[1]))
			desc = stripMD(strings.TrimSpace(m[2]))
		} else {
			title = stripMD(clean)
		}
		if title == "" {
			continue
		}
		block := "[" + title + "](" + raw + ")"
		if desc != "" {
			block += "\n" + desc
		}
		blocks = append(blocks, block)
		used = append(used, raw)
	}
	return strings.Join(blocks, "\n\n"), used
}

// composePackages парсит ответ LLM-секции пакетов. Контракт строки:
//
//	vendor/package — описание <ССЫЛКА>   (описание опц.)
//
// Достаёт URL (matchAllowedURL), отбрасывает строку без валидной ссылки. Формат строки:
// "• [name](url)" + (есть описание ? " — "+desc : ""). Строки через "\n". Возвращает (text, used).
func composePackages(body string, cands []Item) (string, []string) {
	allowed := make(map[string]string, len(cands))
	for _, c := range cands {
		allowed[hashURL(c.Link)] = c.Link
	}
	var lines []string
	var used []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		raw := matchAllowedURL(line, allowed)
		if raw == "" {
			continue
		}
		clean := urlTokenRe.ReplaceAllString(line, "")
		clean = strings.TrimSpace(clean)
		clean = strings.TrimSpace(strings.TrimRight(clean, " —–-"))
		clean = leadingNumRe.ReplaceAllString(clean, "")
		clean = strings.TrimSpace(clean)
		if clean == "" {
			continue
		}
		var name, desc string
		if loc := packageSepRe.FindStringIndex(clean); loc != nil {
			name = stripMD(strings.TrimSpace(clean[:loc[0]]))
			desc = stripMD(strings.TrimSpace(clean[loc[1]:]))
		} else {
			name = stripMD(clean)
		}
		if name == "" {
			continue
		}
		out := "• [" + name + "](" + raw + ")"
		if desc != "" {
			out += " — " + desc
		}
		lines = append(lines, out)
		used = append(used, raw)
	}
	return strings.Join(lines, "\n"), used
}

// assembleDigest собирает финальный текст поста: заголовок + секция статей, затем
// (если есть) секция пакетов.
func assembleDigest(artText, pkgText string) string {
	var b strings.Builder
	b.WriteString("📰 **PHP-дайджест**")
	if artText != "" {
		b.WriteString("\n\n")
		b.WriteString(artText)
	}
	if pkgText != "" {
		b.WriteString("\n\n📦 **Новые пакеты**\n")
		b.WriteString(pkgText)
	}
	return b.String()
}

// matchAllowedURL возвращает последнюю ссылку в строке, совпадающую (после нормализации)
// с одним из кандидатов; иначе "". «Последняя» — т.к. по контракту ссылка стоит в конце
// строки. Толерантна к прилипшей пунктуации вокруг URL, которую мог добавить LLM.
func matchAllowedURL(line string, allowed map[string]string) string {
	last := ""
	for _, m := range urlTokenRe.FindAllString(line, -1) {
		for u := m; ; {
			if raw, ok := allowed[hashURL(u)]; ok {
				last = raw
			}
			trimmed := strings.TrimRight(u, ".,);:!?]>'\"")
			if trimmed == u || trimmed == "" {
				break
			}
			u = trimmed
		}
	}
	return last
}
