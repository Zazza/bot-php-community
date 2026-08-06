package news

import (
	"strings"
	"testing"
	"time"
)

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
	// LLM выбрал пункты 2 и 1 (не по порядку), без ссылок; есть мусорная строка без номера.
	body := "2. **PHP 8.4** — крутой релиз\n\n1. Описание первого\nмусор без номера"
	got := composeDigest(body, cands)
	// ссылка подставлена из кандидата по номеру, порядок как у LLM
	if !strings.Contains(got, "**PHP 8.4** — крутой релиз https://x.com/2") {
		t.Errorf("item 2 link not composed: %s", got)
	}
	if !strings.Contains(got, "Описание первого https://x.com/1") {
		t.Errorf("item 1 link not composed: %s", got)
	}
	if strings.Contains(got, "https://x.com/3") {
		t.Errorf("item 3 should not appear: %s", got)
	}
	if strings.Contains(got, "мусор") {
		t.Errorf("garbage line should be dropped: %s", got)
	}
}

func TestComposeDigestEmptyOnGarbage(t *testing.T) {
	cands := []Item{{Link: "https://x.com/1"}}
	if got := composeDigest("no numbers here at all", cands); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if got := composeDigest("99. out of range", cands); got != "" {
		t.Errorf("out-of-range index should yield empty, got %q", got)
	}
}

func TestParseLeadingIndex(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want int
	}{
		{"3. text", true, 3},
		{"12) text", true, 12},
		{"nope", false, 0},
		{"0. zero", false, 0},
	}
	for _, c := range cases {
		got, ok := parseLeadingIndex(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseLeadingIndex(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

