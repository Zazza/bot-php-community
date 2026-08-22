package news

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"phpbot/internal/md"
	"phpbot/internal/prompts"
)

func TestIsFridaySlot(t *testing.T) {
	// 2026-08-17 = понедельник.
	cases := []struct {
		day  time.Time
		want bool
	}{
		{time.Date(2026, 8, 17, 20, 0, 0, 0, time.UTC), false}, // пн
		{time.Date(2026, 8, 18, 20, 0, 0, 0, time.UTC), false}, // вт
		{time.Date(2026, 8, 19, 20, 0, 0, 0, time.UTC), false}, // ср
		{time.Date(2026, 8, 20, 20, 0, 0, 0, time.UTC), false}, // чт
		{time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC), true},  // пт
		{time.Date(2026, 8, 22, 20, 0, 0, 0, time.UTC), false}, // сб
		{time.Date(2026, 8, 23, 20, 0, 0, 0, time.UTC), false}, // вс
		{time.Date(2026, 8, 21, 0, 1, 0, 0, time.UTC), true},   // ночь пятницы
	}
	for _, c := range cases {
		if got := isFridaySlot(c.day); got != c.want {
			t.Errorf("isFridaySlot(%v) = %v, want %v", c.day, got, c.want)
		}
	}
}

func TestFakeHostAllowed(t *testing.T) {
	cases := []struct {
		link string
		want bool
	}{
		{"https://www.php.net/releases/9.5", true},
		{"https://php.net/rfc/friday-eq", true},
		{"https://github.com/php-vrn/vedro", true},
		{"https://packagist.org/packages/vrn/php-vedro", true},
		{"https://user@github.com/x", false},                 // userinfo — deny
		{"https://sub.packagist.org/packages/x", false},      // поддомен чужой
		{"https://packagist.org.evil.com/packages/x", false}, // суффикс-обманка
		{"https://evil.com/x", false},                        // чужой домен
		{"javascript:alert(1)", false},                       // не URL с хостом
		{"", false},                                          // пусто
		{"подробности в чате", false},                        // не ссылка
	}
	for _, c := range cases {
		if got := fakeHostAllowed(c.link); got != c.want {
			t.Errorf("fakeHostAllowed(%q) = %v, want %v", c.link, got, c.want)
		}
	}
}

func TestUTF16LenMD(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"дайджест", 8},
		{"🎰", 2}, // U+1F570 вне BMP — суррогатная пара
		{"a🎰b", 4},
	}
	for _, c := range cases {
		if got := md.UTF16Len(c.in); got != c.want {
			t.Errorf("UTF16Len(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSanitizeFakeBody(t *testing.T) {
	t.Run("всё валидно — тело не меняется", func(t *testing.T) {
		in := "[Заголовок](https://php.net/x)\nОписание.\n\n📦 **Новые пакеты**\n• [vrn/php-vedro](https://packagist.org/packages/vrn/php-vedro) — выносит прод в ведро"
		if got := sanitizeFakeBody(in); got != in {
			t.Fatalf("sanitize = %q, want исходное", got)
		}
	})
	t.Run("блок с чужим доменом дропается, соседние целы", func(t *testing.T) {
		in := "[A](https://php.net/a) описание\n\n[B](https://evil.com/b) реклама\n\n[C](https://github.com/c) описание"
		got := sanitizeFakeBody(in)
		if strings.Contains(got, "evil.com") || strings.Contains(got, "[B]") {
			t.Fatalf("чужой блок не дропнут: %q", got)
		}
		if !strings.Contains(got, "[A]") || !strings.Contains(got, "[C]") {
			t.Fatalf("валидные блоки потеряны: %q", got)
		}
	})
	t.Run("блок с МЕТА-маркером выдуманности дропается, валидный цел", func(t *testing.T) {
		in := "[Пятничный RFC](https://php.net/a) утечка из LLM\n\n[B](https://github.com/b) описание"
		got := sanitizeFakeBody(in)
		if strings.Contains(got, "Пятничный") || strings.Contains(got, "[A]") {
			t.Fatalf("МЕТА-маркер не дропнут: %q", got)
		}
		if !strings.Contains(got, "[B]") {
			t.Fatalf("валидный блок потерян: %q", got)
		}
	})
	t.Run("чужая схема/регистр ссылки дропается, валидные целы", func(t *testing.T) {
		// Каждый валидный md-матч поглощает "](": оставшийся "](" = непроверенная
		// ссылка (HTTPS://, tg://, javascript:), которую md.ToHTML отрендерит кликабельной.
		in := "[A](https://php.net/a) ок\n\n[B](HTTPS://EVIL.COM/x) капс\n\n[C](tg://resolve?domain=evil) тг\n\n[D](javascript:alert(1)) жс"
		got := sanitizeFakeBody(in)
		if !strings.Contains(got, "[A]") {
			t.Fatalf("валидный блок потерян: %q", got)
		}
		for _, bad := range []string{"EVIL.COM", "tg://", "javascript"} {
			if strings.Contains(got, bad) {
				t.Fatalf("обход белого списка (%s) не дропнут: %q", bad, got)
			}
		}
	})
	t.Run("голый URL без markdown-обёртки дропается", func(t *testing.T) {
		in := "[A](https://php.net/a) ок\n\nЧитайте подробнее на https://evil.com/xxx"
		got := sanitizeFakeBody(in)
		if strings.Contains(got, "evil.com") {
			t.Fatalf("голый URL не дропнут: %q", got)
		}
		if !strings.Contains(got, "[A]") {
			t.Fatalf("валидный блок потерян: %q", got)
		}
	})
	t.Run("ни одной валидной ссылки — пусто", func(t *testing.T) {
		if got := sanitizeFakeBody("**Заголовок**\n\nпросто текст"); got != "" {
			t.Fatalf("sanitize = %q, want \"\"", got)
		}
	})
	t.Run("мусор/пусто", func(t *testing.T) {
		for _, in := range []string{"", "SKIP", "\n\n\n"} {
			if got := sanitizeFakeBody(in); got != "" {
				t.Fatalf("sanitize(%q) = %q, want \"\"", in, got)
			}
		}
	})
}

func TestAssembleFakePost(t *testing.T) {
	t.Run("шапка как у обычного дайджеста, без маркеров выдуманности", func(t *testing.T) {
		got := assembleFakePost("[Статья](https://php.net/x) текст")
		if !strings.HasPrefix(got, "📰 **PHP-дайджест**\n\n") {
			t.Fatalf("заголовок не как у обычного дайджеста: %q", got)
		}
		for _, bad := range []string{"🎰", "пятничн", "вымышлен", "всерьёз"} {
			if strings.Contains(got, bad) {
				t.Fatalf("маркер выдуманности %q в посте: %q", bad, got)
			}
		}
		if !strings.Contains(got, "[Статья](https://php.net/x)") {
			t.Fatalf("тело потеряно: %q", got)
		}
	})
	t.Run("заголовок — общая константа с обычным дайджестом", func(t *testing.T) {
		if !strings.HasPrefix(assembleFakePost("x"), digestTitle) {
			t.Fatalf("digestTitle разошёлся с шапкой фейк-выпуска: %q", digestTitle)
		}
		if !strings.HasPrefix(assembleDigest("a", "p"), digestTitle) {
			t.Fatalf("digestTitle разошёлся с шапкой обычного дайджеста: %q", digestTitle)
		}
	})
	t.Run("гигантское тело ужато в лимит", func(t *testing.T) {
		block := strings.Repeat("а", 5000) + " [x](https://github.com/x)"
		body := strings.Join([]string{block, block, block}, "\n\n")
		got := assembleFakePost(body)
		if n := md.UTF16Len(got); n > fakePostMaxUTF16 {
			t.Fatalf("длина поста = %d UTF-16, want ≤ %d", n, fakePostMaxUTF16)
		}
		if !strings.HasPrefix(got, digestTitle) {
			t.Fatalf("заголовок потерян при обрезке")
		}
	})
}

// TestFakePromptSectionHeaderMatchesDigest — заголовок секции пакетов обязан
// совпадать у обоих выпусков: в обычном дайджесте его пишет код (assembleDigest),
// в пятничном — LLM по шаблону промпта. Расхождение литералов = пятничный выпуск
// визуально отличается от обычного.
func TestFakePromptSectionHeaderMatchesDigest(t *testing.T) {
	const header = "📦 **Новые пакеты**"
	cases := []struct {
		name string
		src  string
	}{
		{"prompt_template", prompts.Get(prompts.FakeNews)},
		{"assembleDigest", assembleDigest("статья", "• [vrn/php-vedro](https://packagist.org/packages/vrn/php-vedro) — выносит прод в ведро")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(c.src, header) {
				t.Errorf("%s: заголовок секции пакетов %q отсутствует — форматы выпусков разошлись:\n%s", c.name, header, c.src)
			}
		})
	}
}

func TestCapFakeBody(t *testing.T) {
	t.Run("в бюджете — без изменений", func(t *testing.T) {
		in := "блок раз\n\nблок два"
		if got := capFakeBody(in, 100); got != in {
			t.Fatalf("capFakeBody = %q, want исходное", got)
		}
	})
	t.Run("над бюджетом — целые блоки и «…»", func(t *testing.T) {
		got := capFakeBody("aaaa\n\nbbbb\n\ncccc", 11) // n=10 после двух блоков, «…» → 11 ровно
		if got != "aaaa\n\nbbbb…" {
			t.Fatalf("capFakeBody = %q, want %q", got, "aaaa\n\nbbbb…")
		}
	})
	t.Run("первый блок-переросток режется", func(t *testing.T) {
		got := capFakeBody(strings.Repeat("б", 100), 10)
		if n := md.UTF16Len(got); n != 10 {
			t.Fatalf("длина = %d, want 10 (обрезка + «…»)", n)
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("нет «…»: %q", got)
		}
	})
	t.Run("точная граница бюджета", func(t *testing.T) {
		got := capFakeBody("abc\n\ndef", 8) // 3+2+3 = 8 — влезает ровно
		if got != "abc\n\ndef" {
			t.Fatalf("точная граница: %q", got)
		}
	})
	t.Run("бюджет заполнен ровно + хвост — без «…», но ≤ budget", func(t *testing.T) {
		got := capFakeBody("abc\n\ndef\n\nx", 8) // после abc+def n=8, «…» не влезает
		if got != "abc\n\ndef" {
			t.Fatalf("хвост при полном бюджете: %q", got)
		}
		if md.UTF16Len(got) > 8 {
			t.Fatalf("нарушен инвариант ≤ budget: %d", md.UTF16Len(got))
		}
	})
	t.Run("руна вне BMP на границе не рвётся пополам", func(t *testing.T) {
		got := capFakeBody("🎰🎰🎰", 3) // 🎰 = 2 units → влезает одна + «…»
		if got != "🎰…" {
			t.Fatalf("суррогатная пара: %q", got)
		}
	})
}

// TestPickRubric — stateless-ротация по ISO-неделе: детерминизм, дни одной недели
// дают одну рубрику (ручной /news fake в вс ≡ cron-слот пятницы), соседние недели —
// разные, 12 последовательных пятниц покрывают пул без повторов.
func TestPickRubric(t *testing.T) {
	monday := time.Date(2026, 1, 5, 20, 0, 0, 0, time.UTC) // пн ISO-недели 2026-W02
	if monday.Weekday() != time.Monday {
		t.Fatalf("премисса: 2026-01-05 = %v, want Monday", monday.Weekday())
	}
	friday := monday.AddDate(0, 0, 4)

	cases := []struct {
		name      string
		a, b      time.Time
		wantEqual bool
	}{
		{"same_date_deterministic", friday, friday, true},
		{"friday_vs_sunday_same_iso_week", friday, monday.AddDate(0, 0, 6), true},
		{"adjacent_iso_weeks_differ", friday, friday.AddDate(0, 0, 7), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ra, rb := pickRubric(c.a), pickRubric(c.b)
			if equal := ra.ID == rb.ID; equal != c.wantEqual {
				t.Errorf("pickRubric(%v) = %q, pickRubric(%v) = %q, wantEqual = %v",
					c.a.Format("2006-01-02"), ra.ID, c.b.Format("2006-01-02"), rb.ID, c.wantEqual)
			}
		})
	}

	t.Run("twelve_fridays_full_pool_no_repeats", func(t *testing.T) {
		seen := make(map[string]struct{}, len(fakeRubrics))
		for i := 0; i < len(fakeRubrics); i++ {
			d := friday.AddDate(0, 0, 7*i)
			if d.Weekday() != time.Friday {
				t.Fatalf("премисса: итерация %d дала %v, want Friday", i, d.Weekday())
			}
			id := pickRubric(d).ID
			if _, dup := seen[id]; dup {
				t.Fatalf("неделя %v: рубрика %q повторилась — ротация сломана", d.Format("2006-01-02"), id)
			}
			seen[id] = struct{}{}
		}
		if len(seen) != len(fakeRubrics) {
			t.Errorf("за %d недель покрыто рубрик = %d, want %d (полный пул)", len(fakeRubrics), len(seen), len(fakeRubrics))
		}
	})

	t.Run("anchor_2026_01_01_iso_week_1_releases", func(t *testing.T) {
		anchor := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		y, w := anchor.ISOWeek()
		if y != 2026 || w != 1 {
			t.Fatalf("премисса: ISOWeek(2026-01-01) = %d-W%02d, want 2026-W01", y, w)
		}
		if want := fakeRubrics[w%len(fakeRubrics)].ID; want != "releases" {
			t.Fatalf("премисса: fakeRubrics[%d].ID = %q, want \"releases\"", w%len(fakeRubrics), want)
		}
		if got := pickRubric(anchor).ID; got != "releases" {
			t.Errorf("pickRubric(2026-01-01).ID = %q, want \"releases\" (ISO week 1)", got)
		}
	})
}

// TestFakeRubricsRender — пул рубрик консистентен: непустые поля (рубрика попадает в
// user-сообщение целиком), уникальные ID (стабильный ключ в news_fake_posts.rubric).
func TestFakeRubricsRender(t *testing.T) {
	if len(fakeRubrics) < 8 {
		t.Errorf("len(fakeRubrics) = %d, want >= 8 (разнообразие ротации)", len(fakeRubrics))
	}
	ids := make(map[string]struct{}, len(fakeRubrics))
	for i, r := range fakeRubrics {
		if r.ID == "" || r.Title == "" || r.Brief == "" || r.Tone == "" {
			t.Errorf("fakeRubrics[%d] (%q): пустое поле ID/Title/Brief/Tone — рубрика уйдёт в LLM неполной", i, r.ID)
		}
		if _, dup := ids[r.ID]; dup {
			t.Errorf("fakeRubrics[%d]: дубликат ID %q — память выпусков не различит рубрики", i, r.ID)
		}
		ids[r.ID] = struct{}{}
	}
}

// TestExtractFakeHeadlines — бан-лист тем: тексты md-ссылок (НЕ URL) из реального
// формата тел выпусков, дедуп с сохранением порядка первого вхождения, кап
// fakeRecentHeadlines, без ссылок — nil.
func TestExtractFakeHeadlines(t *testing.T) {
	var capBody strings.Builder
	var capWant []string
	for i := 0; i < fakeRecentHeadlines+10; i++ {
		h := fmt.Sprintf("Тема выпуска %d", i)
		fmt.Fprintf(&capBody, "[%s](https://php.net/%d)\n", h, i)
		if len(capWant) < fakeRecentHeadlines {
			capWant = append(capWant, h)
		}
	}

	var totalCapBody strings.Builder
	for i := 0; ; i++ {
		h := strings.Repeat("т", 100) // 100 рун каждый: total-кап раньше count-капа
		fmt.Fprintf(&totalCapBody, "[%s](https://php.net/t%d)\n", h, i)
		if (i+1)*100 > fakeRecentTotalRunes {
			break
		}
	}

	cases := []struct {
		name   string
		bodies []string
		want   []string
	}{
		{"real_body_texts_not_urls", []string{
			"[Заголовок статьи](https://github.com/a/b)\nописание статьи\n\n📦 **Новые пакеты**\n• [vendor/package](https://packagist.org/packages/v/n) — выносит прод в ведро",
		}, []string{"Заголовок статьи", "vendor/package"}},
		{"dedup_first_occurrence_order", []string{
			"[A](https://php.net/1) и [B](https://php.net/2)",
			"[B](https://php.net/3), [A](https://php.net/4) и [C](https://php.net/5)",
		}, []string{"A", "B", "C"}},
		{"whitespace_link_text_skipped", []string{
			"[ ](https://php.net/x) пустой текст\n[X](https://php.net/y)",
		}, []string{"X"}},
		{"no_links_nil", []string{"просто текст", "📦 **Новые пакеты**"}, nil},
		{"empty_input_nil", nil, nil},
		{"capped_at_fake_recent_headlines", []string{capBody.String()}, capWant},
		{"capped_at_total_runes", []string{totalCapBody.String()}, nil}, // want проверяется в run-блоке
		{"newline_in_headline_collapsed", []string{
			"[Уязвимость\r\nв ядре](https://php.net/x)",
		}, []string{"Уязвимость в ядре"}},
		{"control_chars_mapped_to_space", []string{
			"[A\x00B\x1fC](https://php.net/x)",
		}, []string{"A B C"}},
		{"headline_capped_at_max_runes", []string{
			"[" + strings.Repeat("я", fakeHeadlineMaxRunes+5) + "](https://php.net/x)",
		}, []string{strings.Repeat("я", fakeHeadlineMaxRunes)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractFakeHeadlines(c.bodies)
			if c.name == "capped_at_total_runes" {
				// total-кап срабатывает раньше count-капа: суммарная длина за пределами
				// бюджета, заголовков меньше, чем ссылок во входе.
				total := 0
				for _, h := range got {
					total += len(h)
				}
				if total > fakeRecentTotalRunes+fakeHeadlineMaxRunes {
					t.Fatalf("total-кап не сработал: %d заголовков, %d рун", len(got), total)
				}
				if len(got) >= fakeRecentHeadlines {
					t.Fatalf("total-кап не сработал раньше count-капа: %d заголовков", len(got))
				}
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("extractFakeHeadlines = %v (len %d), want %v (len %d)",
					got, len(got), c.want, len(c.want))
			}
			for _, h := range got {
				if strings.Contains(h, "http") {
					t.Errorf("в бан-лист попал URL вместо текста: %q", h)
				}
			}
		})
	}
}

// TestBuildFakeUserMessage — user-сообщение: дата, рубрика целиком, ban-лист тем
// построчно; без тем блока «НЕ повторяй» нет; без сырых плейсхолдеров.
func TestBuildFakeUserMessage(t *testing.T) {
	day := time.Date(2026, 1, 9, 20, 0, 0, 0, time.UTC)
	rubric := fakeRubrics[0]
	recent := []string{"Тема один", "Тема два"}

	t.Run("with_recent_full_contract", func(t *testing.T) {
		got := buildFakeUserMessage(day, rubric, recent)
		for _, sub := range []string{"09.01.2026", rubric.Title, rubric.Brief, rubric.Tone, "НЕ повторяй", "- Тема один", "- Тема два"} {
			if !strings.Contains(got, sub) {
				t.Errorf("user-сообщение без %q:\n%s", sub, got)
			}
		}
	})

	t.Run("without_recent_no_banlist_block", func(t *testing.T) {
		for _, rec := range [][]string{nil, {}} {
			got := buildFakeUserMessage(day, rubric, rec)
			if strings.Contains(got, "НЕ повторяй") {
				t.Errorf("recent=%v (пусто), а блок ban-листа тем есть:\n%s", rec, got)
			}
		}
	})

	t.Run("no_raw_placeholders", func(t *testing.T) {
		for _, rec := range [][]string{recent, nil} {
			got := buildFakeUserMessage(day, rubric, rec)
			for _, ph := range []string{"%s", "%d"} {
				if strings.Contains(got, ph) {
					t.Errorf("user-сообщение с сырым плейсхолдером %q:\n%s", ph, got)
				}
			}
		}
	})
}

// TestFakePromptContract — контракт промпта фейк-дайджеста 2.0: статичен (без
// плейсхолдеров — вся динамика идёт user-сообщением, анти-инъекция), SAFETY первой
// строкой, рубрика приходит из user-сообщения. Старая воронка тем 1.0 («Темы
// пародии: …» — фиксированный список, игнорирующий рубрику недели) не должна
// возвращаться. Заголовок секции пакетов «📦 **Новые пакеты» здесь не дублируем —
// его страхует TestFakePromptSectionHeaderMatchesDigest (дрейф промпт↔digest).
func TestFakePromptContract(t *testing.T) {
	p := prompts.Get(prompts.FakeNews)
	if p == "" {
		t.Fatal("fake-news.txt пуст/отсутствует")
	}
	cases := []struct {
		name string
		ok   bool
		msg  string
	}{
		{"safety_first_line", strings.Index(p, "SAFETY:") == 0,
			"SAFETY-блок не первой строкой, want index 0"},
		{"static_no_s_placeholder", !strings.Contains(p, "%s"),
			"промпт содержит \"%s\" — динамика обязана идти user-сообщением, не Sprintf"},
		{"static_no_d_placeholder", !strings.Contains(p, "%d"),
			"промпт содержит \"%d\" — динамика обязана идти user-сообщением, не Sprintf"},
		{"rubric_marker", strings.Contains(p, "в тематике рубрики"),
			"нет маркера «в тематике рубрики» — рубрика недели не доходит до LLM"},
		{"no_old_topic_funnel", !strings.Contains(p, "Темы пародии"),
			"вернулась воронка тем «Темы пародии» из фейк-1.0 — подменяет рубрику недели"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.ok {
				t.Error(c.msg)
			}
		})
	}
}

// TestBodiesOf — маппинг строк памяти выпусков в тела (материал для бан-листа тем):
// порядок сохраняется, пустой вход → пустой слайс.
func TestBodiesOf(t *testing.T) {
	cases := []struct {
		name string
		rows []FakeRow
		want []string
	}{
		{"order_preserved", []FakeRow{{Body: "a"}, {Body: "b"}, {Body: "c"}}, []string{"a", "b", "c"}},
		{"nil_rows", nil, []string{}},
		{"empty_rows", []FakeRow{}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bodiesOf(c.rows)
			if len(got) != len(c.want) {
				t.Fatalf("bodiesOf len = %d, want %d (got %q)", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("bodiesOf[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}
