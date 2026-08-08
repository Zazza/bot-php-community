package news

import (
	"strings"
	"testing"
	"time"

	"github.com/mmcdole/gofeed"
)

// TestDefaultSources — регрессия: новые источники на месте, URL уникальны и валидны.
// Каждый добавленный блог-фид проверен отдельно (см. ADR php-news-digest); случайное
// удаление или дубликат URL сломает тест.
func TestDefaultSources(t *testing.T) {
	srcs := DefaultSources()
	wantBlogs := []string{
		"https://freek.dev/feed",
		"https://www.jamestitcumb.com/feed",
		"https://ocramius.github.io/atom.xml",
		"https://php-testo.github.io/ru/feed.xml",
		"https://tideways.com/feed/atom",
		"https://seld.be/feed.atom",
	}
	byURL := make(map[string]Source, len(srcs))
	for _, s := range srcs {
		if s.URL == "" || s.Name == "" || s.Category == "" {
			t.Errorf("incomplete source: %+v", s)
		}
		switch s.Category {
		case "official", "package", "hub", "blog":
		default:
			t.Errorf("source %q has unknown category %q", s.Name, s.Category)
		}
		if dup, ok := byURL[s.URL]; ok {
			t.Errorf("duplicate source URL %q (%q vs %q)", s.URL, s.Name, dup.Name)
		}
		byURL[s.URL] = s
	}
	for _, u := range wantBlogs {
		if _, ok := byURL[u]; !ok {
			t.Errorf("DefaultSources missing blog feed %q", u)
		} else if byURL[u].Category != "blog" {
			t.Errorf("feed %q category = %q, want blog", u, byURL[u].Category)
		}
	}
	if len(srcs) < 12 {
		t.Errorf("DefaultSources has %d sources, want >= 12 (6 base + 6 blogs)", len(srcs))
	}
}

func TestHashURLNormalization(t *testing.T) {
	cases := []struct{ a, b string }{
		{"https://Example.com/Path/", "https://example.com/Path"},           // host lowercase + trailing slash
		{"https://x.com/a?utm_source=foo&keep=1", "https://x.com/a?keep=1"}, // utm_* stripped
		{"https://x.com/a#frag", "https://x.com/a"},                         // fragment stripped
	}
	for _, c := range cases {
		if hashURL(c.a) != hashURL(c.b) {
			t.Errorf("hashURL(%q) != hashURL(%q): both should normalize to same", c.a, c.b)
		}
	}
	if hashURL("https://x.com/a") == hashURL("https://x.com/b") {
		t.Error("different URLs hashed the same")
	}
	if h := hashURL("https://x.com/a"); len(h) != 40 {
		t.Errorf("sha1 hex length = %d, want 40", len(h))
	}
}

func TestFilterFresh(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	src := Source{Name: "s", URL: "u", Category: "hub"}
	items := []Item{
		{Source: src, Title: "old", Link: "https://x.com/old", Published: now.Add(-8 * 24 * time.Hour)}, // >7д — дроп
		{Source: src, Title: "fresh", Link: "https://x.com/fresh", Published: now.Add(-1 * 24 * time.Hour)},
		{Source: src, Title: "nodate", Link: "https://x.com/nodate", Published: time.Time{}}, // без даты — оставляем
	}
	got := filterFresh(items, now.Add(-freshWindow))
	if len(got) != 2 {
		t.Fatalf("filterFresh kept %d, want 2", len(got))
	}
	for _, it := range got {
		if it.Title == "old" {
			t.Error("old item should be filtered out")
		}
	}
}

func TestDropPosted(t *testing.T) {
	src := Source{Name: "s", URL: "u", Category: "hub"}
	items := []Item{
		{Source: src, Title: "a", Link: "https://x.com/a"},
		{Source: src, Title: "b", Link: "https://x.com/b"}, // posted
		{Source: src, Title: "dup", Link: "https://x.com/a/"}, // нормализуется как a — дубликат в пачке
		{Source: src, Title: "c", Link: "https://x.com/c"},
	}
	posted := map[string]struct{}{hashURL("https://x.com/b"): {}}
	got := dropPosted(items, posted)
	if len(got) != 2 {
		t.Fatalf("dropPosted kept %d, want 2 (a + c, b posted, dup collapsed)", len(got))
	}
	titles := map[string]bool{}
	for _, it := range got {
		titles[it.Title] = true
	}
	if !titles["a"] || !titles["c"] || titles["b"] || titles["dup"] {
		t.Errorf("unexpected survivors: %v", titles)
	}
}

func TestFormatCandidates(t *testing.T) {
	src := Source{Name: "Reddit r/PHP", URL: "u", Category: "hub"}
	items := []Item{
		{Source: src, Title: "Why PHP 8.4 rocks", Link: "https://www.reddit.com/r/php/x", Published: time.Now().Add(-2 * 24 * time.Hour)},
		{Source: src, Title: "NoDate", Link: "https://x.com/n"},
	}
	got := formatCandidates(items)
	for _, want := range []string{"1. [Reddit r/PHP] Why PHP 8.4 rocks", "https://www.reddit.com/r/php/x", "2. [Reddit r/PHP] NoDate", "https://x.com/n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestComposeDigest(t *testing.T) {
	src := Source{Name: "s", URL: "u", Category: "hub"}
	cands := []Item{
		{Source: src, Title: "first", Link: "https://x.com/1"},
		{Source: src, Title: "second", Link: "https://x.com/2"},
		{Source: src, Title: "third", Link: "https://x.com/3"},
	}
	// Регрессия бага «битые ссылки»: LLM оставил ссылку в конце строки, переставил
	// пункты (сначала #2, потом #1) и ПЕРЕНУМЕРОВАЛ их (1., 2.), подсунул чужую ссылку
	// и строку без ссылки. Привязка идёт по URL в строке, а не по номеру → заголовок
	// получает СВОЮ ссылку даже после перенумерации; чужая/отсутствующая — отбрасываются.
	body := "1. **PHP 8.4** — крутой релиз https://x.com/2\n\n" +
		"2. Описание первого https://x.com/1\n" +
		"левак с подменной ссылкой https://evil.com/x\n" +
		"строка вообще без ссылки"
	got := composeDigest(body, cands)
	if !strings.Contains(got, "**PHP 8.4** — крутой релиз [🔗](https://x.com/2)") {
		t.Errorf("item should keep ITS url despite LLM renumbering:\n%s", got)
	}
	if !strings.Contains(got, "Описание первого [🔗](https://x.com/1)") {
		t.Errorf("item should keep ITS url:\n%s", got)
	}
	if !strings.Contains(got, "[🔗](https://x.com/2)\n\nОписание первого") {
		t.Errorf("items should be separated by blank line:\n%s", got)
	}
	if strings.Contains(got, "evil.com") {
		t.Errorf("non-candidate URL must NOT leak into post:\n%s", got)
	}
	if strings.Contains(got, "без ссылки") {
		t.Errorf("linkless line should be dropped:\n%s", got)
	}
	if strings.Contains(got, "https://x.com/3") {
		t.Errorf("unselected item 3 should not appear:\n%s", got)
	}
}

func TestComposeDigestEmptyOnGarbage(t *testing.T) {
	cands := []Item{{Link: "https://x.com/1"}}
	if got := composeDigest("вообще без ссылок", cands); got != "" {
		t.Errorf("no URL → expected empty, got %q", got)
	}
	if got := composeDigest("desc https://not-a-candidate.com", cands); got != "" {
		t.Errorf("non-candidate URL → expected empty, got %q", got)
	}
}

// TestSanitizeURLAttrs — триммит пробелы в href/url, прочие атрибуты не трогает.
// Регрессия для php.net-подобных фидов с href=" https://..." (ронял весь фид).
func TestSanitizeURLAttrs(t *testing.T) {
	cases := []struct{ in, want string }{
		{`href=" https://x"`, `href="https://x"`},
		{`href="https://y "`, `href="https://y"`},
		{`url=" https://z "`, `url="https://z"`},
		{`HREF=" https://case"`, `HREF="https://case"`},
		{`href="https://ok"`, `href="https://ok"`},            // чистый — без изменений
		{`class=" keep me "`, `class=" keep me "`},            // не URL-атрибут — нетронуто
		{`<link href=" https://a"/> <a href=" https://b">`, `<link href="https://a"/> <a href="https://b">`},
	}
	for _, c := range cases {
		if got := sanitizeURLAttrs(c.in); got != c.want {
			t.Errorf("sanitizeURLAttrs(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

// TestFeedParseRecoversFromBadURL — реальный атом-фид с битым href: gofeed падает на
// сыром теле, но после sanitizeURLAttrs парсится и отдаёт элементы с корректными ссылками.
func TestFeedParseRecoversFromBadURL(t *testing.T) {
	raw := `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>t</title>
<link href=" https://loop-run.io"/>
<entry><title>Entry one</title><link href=" https://example.com/a"/><id>a</id></entry>
<entry><title>Entry two</title><link href="https://example.com/b"/><id>b</id></entry>
</feed>`
	p := gofeed.NewParser()
	if _, err := p.ParseString(raw); err == nil {
		t.Fatal("expected raw feed with bad href to fail primary parse, got nil")
	}
	feed, err := p.ParseString(sanitizeURLAttrs(raw))
	if err != nil {
		t.Fatalf("re-parse after sanitize failed: %v", err)
	}
	if len(feed.Items) != 2 {
		t.Fatalf("expected 2 items recovered, got %d", len(feed.Items))
	}
	want := map[string]string{"Entry one": "https://example.com/a", "Entry two": "https://example.com/b"}
	for _, it := range feed.Items {
		if it.Link != want[it.Title] {
			t.Errorf("item %q link = %q, want %q", it.Title, it.Link, want[it.Title])
		}
	}
}

