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
		{Source: src, Title: "b", Link: "https://x.com/b"},    // posted
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

// TestSanitizeURLAttrs — триммит пробелы в href/url, прочие атрибуты не трогает.
// Регрессия для php.net-подобных фидов с href=" https://..." (ронял весь фид).
func TestSanitizeURLAttrs(t *testing.T) {
	cases := []struct{ in, want string }{
		{`href=" https://x"`, `href="https://x"`},
		{`href="https://y "`, `href="https://y"`},
		{`url=" https://z "`, `url="https://z"`},
		{`HREF=" https://case"`, `HREF="https://case"`},
		{`href="https://ok"`, `href="https://ok"`}, // чистый — без изменений
		{`class=" keep me "`, `class=" keep me "`}, // не URL-атрибут — нетронуто
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

// TestFormatCandidates — новый формат строки кандидата для LLM с тегом категории:
// "N. [category] Source Name — Title (Xд назад) — link". Item без даты → без суффикса возраста.
func TestFormatCandidates(t *testing.T) {
	items := []Item{
		{Source: Source{Name: "Freek", URL: "f", Category: "blog"}, Title: "Blog Post", Link: "https://freek.dev/1", Published: time.Now().Add(-3 * 24 * time.Hour)},
		{Source: Source{Name: "Packagist", URL: "p", Category: "package"}, Title: "New Package", Link: "https://packagist.org/p/a", Published: time.Now().Add(-1 * 24 * time.Hour)},
		{Source: Source{Name: "NoDate", URL: "n", Category: "hub"}, Title: "No Date Item", Link: "https://x.com/nodate"},
	}
	got := formatCandidates(items)
	mustContain := []string{
		"1. [blog] Freek",
		"Blog Post",
		"https://freek.dev/1",
		"[package]",
		"New Package",
		"https://packagist.org/p/a",
		"[hub] NoDate",
		"https://x.com/nodate",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("formatCandidates missing %q in:\n%s", sub, got)
		}
	}
	// Датированный item → суффикс возраста "(Xд назад)".
	if !strings.Contains(got, "Blog Post (") {
		t.Errorf("dated item should have age suffix in:\n%s", got)
	}
	// Item без даты → без суффикса возраста.
	if strings.Contains(got, "No Date Item (") {
		t.Errorf("undated item should have no age suffix in:\n%s", got)
	}
}

// TestComposeArticles — ключевой регресс: защита от URL-инъекции (чужая ссылка → строку
// пропускаем) + новый формат вывода "[Заголовок](url)\nОписание" (НЕ старый **…** — [🔗]).
// Контракт строки LLM: **Заголовок** — описание <ССЫЛКА>.
func TestComposeArticles(t *testing.T) {
	src := Source{Name: "s", URL: "u", Category: "hub"}
	cands := []Item{
		{Source: src, Title: "a", Link: "https://x.com/1"},
		{Source: src, Title: "b", Link: "https://x.com/2"},
		{Source: src, Title: "c", Link: "https://x.com/3"}, // невыбранный кандидат
	}
	body := strings.Join([]string{
		"1. **PHP 8.4** — крутой релиз https://x.com/2",
		"2. Описание первого https://x.com/1",
		"левак с подменной ссылкой https://evil.com/x",
		"строка вообще без ссылки",
	}, "\n")
	text, used := composeArticles(body, cands)

	// Заголовок = ссылка, описание отдельной строкой (новый формат).
	if !strings.Contains(text, "[PHP 8.4](https://x.com/2)\nкрутой релиз") {
		t.Errorf("expected heading-as-link + description block; got:\n%s", text)
	}
	// Нет ** — title = весь текст, без описания.
	if !strings.Contains(text, "[Описание первого](https://x.com/1)") {
		t.Errorf("expected title-as-whole-text block; got:\n%s", text)
	}
	// Блоки разделены пустой строкой.
	if !strings.Contains(text, "\n\n") {
		t.Errorf("blocks should be separated by \\n\\n; got:\n%s", text)
	}
	// used — РАВНЫЕ url (не хэши), порядок сохранён.
	wantUsed := []string{"https://x.com/2", "https://x.com/1"}
	if got := strings.Join(used, "|"); got != strings.Join(wantUsed, "|") {
		t.Errorf("used = %v, want %v", used, wantUsed)
	}
	// Чужая ссылка отбрасывается (защита от подмены URL) — вся строка пропускается.
	if strings.Contains(text, "evil.com") {
		t.Errorf("foreign URL leaked into text:\n%s", text)
	}
	// Строка без ссылки пропущена.
	if strings.Contains(text, "без ссылки") {
		t.Errorf("no-link line should be dropped:\n%s", text)
	}
	// Невыбранный кандидат не должен появиться ни в text, ни в used.
	if strings.Contains(text, "https://x.com/3") {
		t.Errorf("unselected candidate URL leaked into text:\n%s", text)
	}
}

// TestComposeArticlesEmptyOnGarbage — fail-safe: мусорный ответ LLM → пустая секция,
// ничего не отмечается как used (второй бакет/пост не падает из-за одной секции).
func TestComposeArticlesEmptyOnGarbage(t *testing.T) {
	src := Source{Name: "s", URL: "u", Category: "hub"}
	cands := []Item{{Source: src, Title: "a", Link: "https://x.com/1"}}

	// Body без ссылок → пусто.
	text, used := composeArticles("просто текст без единой ссылки", cands)
	if text != "" || len(used) != 0 {
		t.Errorf("no links: text=%q used=%v, want empty", text, used)
	}

	// Body с чужой ссылкой (не из кандидатов) → пусто.
	text, used = composeArticles("текст с подменной https://evil.com/x", cands)
	if text != "" || len(used) != 0 {
		t.Errorf("foreign link: text=%q used=%v, want empty", text, used)
	}
}

// TestComposePackages — парсинг секции пакетов. Контракт строки LLM:
// vendor/package — описание <ССЫЛКА> (описание опц.). Формат: "• [name](url)" + опц. " — desc",
// строки через один \n (compact, не \n\n как в статьях).
func TestComposePackages(t *testing.T) {
	src := Source{Name: "s", URL: "u", Category: "package"}
	cands := []Item{
		{Source: src, Title: "a", Link: "https://p.com/a"},
		{Source: src, Title: "b", Link: "https://p.com/b"},
	}
	body := strings.Join([]string{
		"symfony/console — компонент для CLI https://p.com/a",
		"infection/infection https://p.com/b",
		"мусор https://evil.com/x",
	}, "\n")
	text, used := composePackages(body, cands)

	if !strings.Contains(text, "• [symfony/console](https://p.com/a) — компонент для CLI") {
		t.Errorf("missing package line with description; got:\n%s", text)
	}
	if !strings.Contains(text, "• [infection/infection](https://p.com/b)") {
		t.Errorf("missing package line without description; got:\n%s", text)
	}
	// Пакеты разделены одним \n (не \n\n — compact-формат).
	if strings.Contains(text, "\n\n") {
		t.Errorf("package lines should be \\n-separated, not \\n\\n; got:\n%s", text)
	}
	wantUsed := []string{"https://p.com/a", "https://p.com/b"}
	if got := strings.Join(used, "|"); got != strings.Join(wantUsed, "|") {
		t.Errorf("used = %v, want %v", used, wantUsed)
	}
	if strings.Contains(text, "evil.com") {
		t.Errorf("foreign URL leaked into packages:\n%s", text)
	}
}

// TestDropPreReleases — фильтр stable-only для бакета пакетов: pre-release версии
// (alpha/beta/rc/dev/snapshot/pl с цифрой перед маркером) отбрасываются, stable — остаются.
// Регрессия на false-positive: «archive», «transaction», «models-dev» (буква перед маркером).
func TestDropPreReleases(t *testing.T) {
	src := Source{Name: "s", URL: "u", Category: "package"}
	items := []Item{
		{Source: src, Title: "PHP 8.4.0RC1 released", Link: "https://x.com/1"},       // drop (RC)
		{Source: src, Title: "v1.2.3-rc1", Link: "https://x.com/2"},                  // drop (rc)
		{Source: src, Title: "7.1.0beta2", Link: "https://x.com/3"},                  // drop (beta)
		{Source: src, Title: "3.0.0-alpha.1", Link: "https://x.com/4"},               // drop (alpha)
		{Source: src, Title: "PHP 8.5.9 released", Link: "https://x.com/5"},          // keep (stable)
		{Source: src, Title: "vendor/package (1.0.0)", Link: "https://x.com/6"},      // keep (stable)
		{Source: src, Title: "archive of releases", Link: "https://x.com/7"},         // keep («rc» без цифры перед)
		{Source: src, Title: "transaction support", Link: "https://x.com/8"},         // keep (false-positive ctrl)
		{Source: src, Title: "symfony/models-dev (v127.0)", Link: "https://x.com/9"}, // keep («dev» без цифры перед)
	}
	got := dropPreReleases(items)
	if dropped := len(items) - len(got); dropped != 4 {
		t.Errorf("dropped %d pre-releases, want 4", dropped)
	}
	if len(got) != 5 {
		t.Fatalf("dropPreReleases kept %d, want 5", len(got))
	}
	kept := make(map[string]bool, len(got))
	for _, it := range got {
		kept[it.Title] = true
	}
	wantKept := []string{
		"PHP 8.5.9 released",
		"vendor/package (1.0.0)",
		"archive of releases",
		"transaction support",
		"symfony/models-dev (v127.0)",
	}
	for _, title := range wantKept {
		if !kept[title] {
			t.Errorf("stable title %q was dropped (false-positive of pre-release regex)", title)
		}
	}
}

// TestSplitByBucket — деление по Source.Category: package → packages, остальные → articles.
// Неизвестная категория → articles (fail-safe: лучше показать, чем потерять).
func TestSplitByBucket(t *testing.T) {
	item := func(cat string) Item {
		return Item{Source: Source{Name: cat, URL: cat, Category: cat}, Title: cat, Link: "https://x.com/" + cat}
	}
	items := []Item{
		item("official"),
		item("hub"),
		item("blog"),
		item("package"),
		item("foo"), // неизвестная → articles (fail-safe)
	}
	articles, packages := splitByBucket(items)
	if len(packages) != 1 || packages[0].Source.Category != "package" {
		t.Errorf("packages = %+v, want exactly one package", packages)
	}
	if len(articles) != 4 {
		t.Fatalf("articles len = %d, want 4 (official+hub+blog+foo)", len(articles))
	}
	cats := make(map[string]bool, len(articles))
	for _, a := range articles {
		cats[a.Source.Category] = true
	}
	for _, want := range []string{"official", "hub", "blog", "foo"} {
		if !cats[want] {
			t.Errorf("articles missing category %q (unknown must fall back to articles)", want)
		}
	}
}

// TestSortByArticlesPriority — порядок статей: blog → official → hub, внутри категории
// исходный взаимный порядок сохраняется (sort.SliceStable). Возвращает новый срез.
func TestSortByArticlesPriority(t *testing.T) {
	item := func(cat, title string) Item {
		return Item{Source: Source{Name: cat, URL: cat, Category: cat}, Title: title, Link: "https://x.com/" + title}
	}
	items := []Item{
		item("hub", "h1"),
		item("blog", "b1"),
		item("official", "o1"),
		item("blog", "b2"),
		item("hub", "h2"),
	}
	got := sortByArticlesPriority(items)
	wantCats := []string{"blog", "blog", "official", "hub", "hub"}
	if len(got) != len(wantCats) {
		t.Fatalf("sortByArticlesPriority len = %d, want %d", len(got), len(wantCats))
	}
	for i, want := range wantCats {
		if got[i].Source.Category != want {
			t.Errorf("pos %d category = %q, want %q (full order: %v)", i, got[i].Source.Category, want, catsOf(got))
		}
	}
	// Стабильность: b1 остался раньше b2 (исходный взаимный порядок двух blog).
	var b1Idx, b2Idx = -1, -1
	for i, it := range got {
		switch it.Title {
		case "b1":
			b1Idx = i
		case "b2":
			b2Idx = i
		}
	}
	if b1Idx < 0 || b2Idx < 0 {
		t.Fatalf("lost blog items after sort: b1@%d b2@%d", b1Idx, b2Idx)
	}
	if b1Idx > b2Idx {
		t.Errorf("stable order broken: b1@%d should come before b2@%d", b1Idx, b2Idx)
	}
}

func catsOf(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Source.Category
	}
	return out
}

// TestAssembleDigest — сборка финального поста: заголовок + статьи + опц. секция пакетов.
// Регресс формата: при пустых статьях перед «📦 Новые пакеты» ровно один \n\n (не два).
func TestAssembleDigest(t *testing.T) {
	// Полный пост: заголовок + статьи + пакеты.
	full := assembleDigest("art", "pkg")
	for _, sub := range []string{"📰 **PHP-дайджест**", "art", "📦 **Новые пакеты**", "pkg"} {
		if !strings.Contains(full, sub) {
			t.Errorf("full digest missing %q:\n%s", sub, full)
		}
	}

	// Только статьи, секции пакетов нет.
	artOnly := assembleDigest("art", "")
	if !strings.Contains(artOnly, "📰 **PHP-дайджест**") || !strings.Contains(artOnly, "art") {
		t.Errorf("articles-only missing header/article:\n%s", artOnly)
	}
	if strings.Contains(artOnly, "📦") {
		t.Errorf("articles-only should not include packages section:\n%s", artOnly)
	}

	// Статьи пусты, пакеты есть: ОДИН \n\n перед секцией пакетов (не двойной пустой строкой).
	pkgOnly := assembleDigest("", "pkg")
	if !strings.Contains(pkgOnly, "📦 **Новые пакеты**") || !strings.Contains(pkgOnly, "pkg") {
		t.Errorf("packages-only missing packages section:\n%s", pkgOnly)
	}
	if !strings.Contains(pkgOnly, "\n\n📦") {
		t.Errorf("expected single \\n\\n before packages section:\n%s", pkgOnly)
	}
	if strings.Contains(pkgOnly, "\n\n\n\n📦") {
		t.Errorf("found double blank line before packages (want single \\n\\n):\n%s", pkgOnly)
	}

	// Всё пусто → только заголовок.
	empty := assembleDigest("", "")
	if want := "📰 **PHP-дайджест**"; empty != want {
		t.Errorf("empty digest = %q, want %q", empty, want)
	}
}

// TestLeadingNumRePreservesVersion — регресс: ведущая нумерация требует пробел после маркера
// (\s+), поэтому заголовок-версия без ** обёртки ("8.4 Release") НЕ калечится в "4 Release".
func TestLeadingNumRePreservesVersion(t *testing.T) {
	src := Source{Name: "s", URL: "u", Category: "hub"}
	cands := []Item{{Source: src, Title: "v", Link: "https://x.com/v"}}
	// LLM нарушил контракт: не завернул в ** и строка начинается с версии.
	text, used := composeArticles("8.4 Release — new features https://x.com/v", cands)
	if !strings.Contains(text, "[8.4 Release") {
		t.Errorf("version title mangled by leadingNumRe; got:\n%s", text)
	}
	if strings.Contains(text, "[4 Release") {
		t.Errorf("leadingNumRe stripped \"8.\" from version title; got:\n%s", text)
	}
	if len(used) != 1 || used[0] != "https://x.com/v" {
		t.Errorf("used = %v, want [https://x.com/v]", used)
	}
}

// TestComposeArticlesSanitizesBrackets — регресс security/logic WARN: LLM-контролируемый
// title/desc очищается от квадратных скобок — не может ни разорвать обёртку [..](..)
// (утянутый тег [blog]), ни собрать ссылку с произвольной схемой ([x](javascript:...)).
func TestComposeArticlesSanitizesBrackets(t *testing.T) {
	src := Source{Name: "s", URL: "u", Category: "hub"}
	cands := []Item{
		{Source: src, Title: "b", Link: "https://x.com/b"},
		{Source: src, Title: "c", Link: "https://x.com/c"},
	}
	body := strings.Join([]string{
		"**[blog] Real Title** — desc https://x.com/b",
		"**T** — see [x](javascript:alert(1)) https://x.com/c",
	}, "\n")
	text, _ := composeArticles(body, cands)

	// Тег [blog] в заголовке вырезан — обёртка ссылки цела, без «[[blog]».
	if !strings.Contains(text, "[blog Real Title](https://x.com/b)") {
		t.Errorf("bracket tag should be stripped, link wrapper intact; got:\n%s", text)
	}
	if strings.Contains(text, "[[blog]") {
		t.Errorf("double bracket leaked (would break mdToHTML); got:\n%s", text)
	}
	// Описание со схемой-инъекцией: скобки вырезаны, ссылка не собирается.
	if strings.Contains(text, "[x]") || strings.Contains(text, "[javascript") {
		t.Errorf("bracket link pattern leaked into description; got:\n%s", text)
	}
}

// TestComposePackagesSanitizesBrackets — то же для секции пакетов: name/desc без скобок,
// обёртка ссылки и имя пакета не рвутся.
func TestComposePackagesSanitizesBrackets(t *testing.T) {
	src := Source{Name: "s", URL: "u", Category: "package"}
	cands := []Item{{Source: src, Title: "x", Link: "https://p.com/x"}}
	text, _ := composePackages("pkg/name — d [x](ftp://y) https://p.com/x", cands)
	if strings.Contains(text, "[x]") {
		t.Errorf("bracket link pattern leaked into package description; got:\n%s", text)
	}
	if !strings.Contains(text, "• [pkg/name](https://p.com/x)") {
		t.Errorf("package line missing or wrapper broken; got:\n%s", text)
	}
}
