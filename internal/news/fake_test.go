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
	t.Run("текст без ссылок (вступление/заключение) дропается, статьи целы", func(t *testing.T) {
		in := "Вот и пятница, собрали для вас подборку!\n\n[A](https://php.net/a) описание\n\nСпасибо, что читаете."
		got := sanitizeFakeBody(in)
		if strings.Contains(got, "подборку") || strings.Contains(got, "Спасибо") {
			t.Fatalf("бесконтентный текст не дропнут: %q", got)
		}
		if !strings.Contains(got, "[A]") {
			t.Fatalf("статья потеряна: %q", got)
		}
	})
	t.Run("отдельный блок-заголовок секции пакетов сохраняется", func(t *testing.T) {
		in := "[A](https://php.net/a) описание\n\n📦 **Новые пакеты**\n\n• [v/p](https://packagist.org/packages/v/p) — desc"
		got := sanitizeFakeBody(in)
		if !strings.Contains(got, "📦 **Новые пакеты**") {
			t.Fatalf("заголовок секции пакетов потерян: %q", got)
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

// TestPickFakePlan — stateless-план «2+2» по ISO-неделе: детерминизм, дни одной
// недели дают один план целиком (ручной /news fake в вс ≡ cron-слот пятницы),
// соседние недели меняют и якорь, и extras, последовательные пятницы покрывают
// пул якорей без повторов, extras следуют смещениям +3/+7 от номера недели,
// все три рубрики плана попарно различны (в т.ч. на стыках цикла).
func TestPickFakePlan(t *testing.T) {
	friday := time.Date(2026, 1, 2, 20, 0, 0, 0, time.UTC) // пятница ISO-недели 2026-W01
	if friday.Weekday() != time.Friday {
		t.Fatalf("премисса: 2026-01-02 = %v, want Friday", friday.Weekday())
	}
	if y, w := friday.ISOWeek(); y != 2026 || w != 1 {
		t.Fatalf("премисса: ISOWeek(2026-01-02) = %d-W%02d, want 2026-W01", y, w)
	}

	cases := []struct {
		name      string
		a, b      time.Time
		wantEqual bool
	}{
		{"same_date_deterministic", friday, friday, true},
		{"friday_vs_sunday_same_iso_week", friday, friday.AddDate(0, 0, 2), true},
		{"adjacent_iso_weeks_differ", friday, friday.AddDate(0, 0, 7), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pa, pb := pickFakePlan(c.a), pickFakePlan(c.b)
			if equal := reflect.DeepEqual(pa, pb); equal != c.wantEqual {
				t.Errorf("планы %v/%v equal = %v, want %v (якоря %q/%q)",
					c.a.Format("2006-01-02"), c.b.Format("2006-01-02"), equal, c.wantEqual, pa.Anchor.ID, pb.Anchor.ID)
			}
			if c.wantEqual {
				return
			}
			if pa.Anchor.ID == pb.Anchor.ID {
				t.Errorf("планы %v/%v: якорь не сменился (%q)",
					c.a.Format("2006-01-02"), c.b.Format("2006-01-02"), pa.Anchor.ID)
			}
			if reflect.DeepEqual(pa.Extras, pb.Extras) {
				t.Errorf("планы %v/%v: extras не сменились (%q, %q)",
					c.a.Format("2006-01-02"), c.b.Format("2006-01-02"), pa.Extras[0].ID, pa.Extras[1].ID)
			}
		})
	}

	t.Run("consecutive_fridays_full_pool", func(t *testing.T) {
		seen := make(map[string]struct{}, len(fakeRubrics))
		for i := 0; i < len(fakeRubrics); i++ {
			d := friday.AddDate(0, 0, 7*i)
			if d.Weekday() != time.Friday {
				t.Fatalf("премисса: итерация %d дала %v, want Friday", i, d.Weekday())
			}
			id := pickFakePlan(d).Anchor.ID
			if _, dup := seen[id]; dup {
				t.Fatalf("неделя %v: якорь %q повторился — ротация сломана", d.Format("2006-01-02"), id)
			}
			seen[id] = struct{}{}
		}
		if len(seen) != len(fakeRubrics) {
			t.Errorf("за %d недель покрыто якорей = %d, want %d (полный пул)", len(fakeRubrics), len(seen), len(fakeRubrics))
		}
	})

	t.Run("anchor_2026_01_01_releases", func(t *testing.T) {
		anchor := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		y, w := anchor.ISOWeek()
		if y != 2026 || w != 1 {
			t.Fatalf("премисса: ISOWeek(2026-01-01) = %d-W%02d, want 2026-W01", y, w)
		}
		if want := fakeRubrics[w%len(fakeRubrics)].ID; want != "releases" {
			t.Fatalf("премисса: fakeRubrics[%d].ID = %q, want \"releases\"", w%len(fakeRubrics), want)
		}
		if got := pickFakePlan(anchor).Anchor.ID; got != "releases" {
			t.Errorf("pickFakePlan(2026-01-01).Anchor.ID = %q, want \"releases\" (ISO week 1)", got)
		}
	})

	t.Run("extras_contract_offsets", func(t *testing.T) {
		// Формулу смещений проверяем от ISOWeek самой даты, ID — из самого пула.
		weeks := make(map[int]struct{})
		for i := 0; i < len(fakeRubrics)+2; i++ {
			d := friday.AddDate(0, 0, 7*i)
			_, w := d.ISOWeek()
			weeks[w] = struct{}{}
			got := pickFakePlan(d)
			if len(got.Extras) != 2 {
				t.Fatalf("неделя %d: len(Extras) = %d, want 2", w, len(got.Extras))
			}
			want0 := fakeRubrics[(w+3)%len(fakeRubrics)].ID
			want1 := fakeRubrics[(w+7)%len(fakeRubrics)].ID
			if got.Extras[0].ID != want0 || got.Extras[1].ID != want1 {
				t.Errorf("неделя %d: Extras = [%s, %s], want [%s, %s] (смещения +3/+7)",
					w, got.Extras[0].ID, got.Extras[1].ID, want0, want1)
			}
		}
		for _, seam := range []int{10, 11, 12} { // стык цикла (w%len заворачивает) покрыт выборкой
			if _, ok := weeks[seam]; !ok {
				t.Errorf("премисса: стык цикла w=%d не покрыт выборкой недель", seam)
			}
		}
	})

	t.Run("three_distinct_rubrics", func(t *testing.T) {
		for i := 0; i < len(fakeRubrics)+2; i++ {
			d := friday.AddDate(0, 0, 7*i)
			_, w := d.ISOWeek()
			p := pickFakePlan(d)
			ids := []string{p.Anchor.ID, p.Extras[0].ID, p.Extras[1].ID}
			for a := 0; a < len(ids); a++ {
				for b := a + 1; b < len(ids); b++ {
					if ids[a] == ids[b] {
						t.Errorf("неделя %d: рубрики плана совпали (%q) — якорь и extras обязаны быть тремя разными",
							w, ids[a])
					}
				}
			}
		}
	})
}

// TestFakeRubricKey — ключ плана «anchor+extra1+extra2» для news_fake_posts.rubric:
// состав недели 2026-W01 (якорь releases + две extras по смещениям +3/+7), якорь
// первым, ровно три компоненты, детерминизм. ID extras вычисляются из самого пула —
// строки extras в тесте не захардкожены.
func TestFakeRubricKey(t *testing.T) {
	day := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	_, w := day.ISOWeek()
	if w != 1 {
		t.Fatalf("премисса: ISOWeek(2026-01-01) = %d, want 1", w)
	}
	if anchor := fakeRubrics[w%len(fakeRubrics)].ID; anchor != "releases" {
		t.Fatalf("премисса: fakeRubrics[%d].ID = %q, want \"releases\"", w%len(fakeRubrics), anchor)
	}
	want := strings.Join([]string{
		"releases",
		fakeRubrics[(w+3)%len(fakeRubrics)].ID,
		fakeRubrics[(w+7)%len(fakeRubrics)].ID,
	}, "+")

	t.Run("week1_releases_plus_extras", func(t *testing.T) {
		if got := fakeRubricKey(pickFakePlan(day)); got != want {
			t.Errorf("fakeRubricKey(2026-01-01) = %q, want %q", got, want)
		}
	})

	friday := time.Date(2026, 1, 2, 20, 0, 0, 0, time.UTC) // пятница ISO-недели 2026-W01
	t.Run("anchor_first_three_parts", func(t *testing.T) {
		for i := 0; i < len(fakeRubrics)+2; i++ {
			d := friday.AddDate(0, 0, 7*i)
			p := pickFakePlan(d)
			key := fakeRubricKey(p)
			if !strings.HasPrefix(key, p.Anchor.ID) {
				t.Errorf("%v: ключ %q не начинается с якоря %q", d.Format("2006-01-02"), key, p.Anchor.ID)
			}
			if !strings.HasSuffix(key, p.Extras[len(p.Extras)-1].ID) {
				t.Errorf("%v: ключ %q не кончается последней extra %q", d.Format("2006-01-02"), key, p.Extras[len(p.Extras)-1].ID)
			}
			if n := strings.Count(key, "+"); n != len(p.Extras) {
				t.Errorf("%v: ключ %q: разделителей \"+\" = %d, want %d", d.Format("2006-01-02"), key, n, len(p.Extras))
			}
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		if fakeRubricKey(pickFakePlan(day)) != fakeRubricKey(pickFakePlan(day)) {
			t.Error("ключ плана не детерминирован для одной даты")
		}
	})
}

// TestFakeRubricsRender — пул рубрик консистентен: непустые поля (рубрика попадает в
// user-сообщение целиком), уникальные ID (стабильный ключ в news_fake_posts.rubric),
// поля без МЕТА-маркеров выдуманности (Brief/Tone уходят в user-сообщение — эхо
// маркера в тело дропнулось бы санитайзером).
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
		for _, field := range []string{r.Title, r.Brief, r.Tone} {
			low := strings.ToLower(field)
			for _, m := range fakeMetaMarkers {
				if strings.Contains(low, m) {
					t.Errorf("fakeRubrics[%d] (%q): МЕТА-маркер %q в поле рубрики — эхо в теле дропнет блок", i, r.ID, m)
				}
			}
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
		// 100 рун и уникальный текст: иначе дедуп схлопнет всё в один заголовок и
		// total-кап не сработает.
		h := fmt.Sprintf("%s %d", strings.Repeat("т", 97), i)
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
					total += len([]rune(h))
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

// TestBuildFakeUserMessage — user-сообщение: дата, распределение статей по рубрикам
// плана «2+2» (якорь — статьи 1 и 2, extras — статьи 3 и 4, в этом порядке), пакеты
// вне рубрик, ban-лист тем построчно; без тем блока «НЕ повторяй» нет; без сырых
// плейсхолдеров.
func TestBuildFakeUserMessage(t *testing.T) {
	day := time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC) // пятница ISO-недели 2026-W34
	plan := fakePlan{Anchor: fakeRubrics[0], Extras: []fakeRubric{fakeRubrics[1], fakeRubrics[2]}}
	recent := []string{"Тема один", "Тема два"}

	t.Run("full_contract", func(t *testing.T) {
		got := buildFakeUserMessage(day, plan, recent)
		must := []string{
			"21.08.2026",
			"Распределение статей по рубрикам:",
			"- Статьи 1 и 2 — якорная рубрика выпуска",
			"- Статья 3 — рубрика",
			"- Статья 4 (если она есть) — рубрика",
			"Пакеты — без привязки к рубрикам: любая тема PHP-экосистемы.",
			plan.Anchor.Title, plan.Anchor.Brief, plan.Anchor.Tone,
			plan.Extras[0].Title, plan.Extras[0].Tone,
			plan.Extras[1].Title, plan.Extras[1].Tone,
			"Темы прошлых выпусков — НЕ повторяй их и близкие вариации:",
			"- Тема один", "- Тема два",
		}
		for _, sub := range must {
			if !strings.Contains(got, sub) {
				t.Errorf("user-сообщение без %q:\n%s", sub, got)
			}
		}
	})

	t.Run("anchor_first", func(t *testing.T) {
		got := buildFakeUserMessage(day, plan, recent)
		anchor := strings.Index(got, "- Статьи 1 и 2 — якорная рубрика выпуска")
		third := strings.Index(got, "- Статья 3 — рубрика")
		fourth := strings.Index(got, "- Статья 4 (если она есть) — рубрика")
		if anchor < 0 || third < 0 || fourth < 0 {
			t.Fatalf("нет строк распределения: якорь=%d статья3=%d статья4=%d\n%s", anchor, third, fourth, got)
		}
		if !(anchor < third && third < fourth) {
			t.Errorf("порядок рубрик: якорь=%d, статья3=%d, статья4=%d, want якорь < статья3 < статья4",
				anchor, third, fourth)
		}
	})

	t.Run("without_recent_no_banlist_block", func(t *testing.T) {
		for _, rec := range [][]string{nil, {}} {
			got := buildFakeUserMessage(day, plan, rec)
			if strings.Contains(got, "НЕ повторяй") {
				t.Errorf("recent=%v (пусто), а блок ban-листа тем есть:\n%s", rec, got)
			}
		}
	})

	t.Run("no_raw_placeholders", func(t *testing.T) {
		for _, rec := range [][]string{recent, nil} {
			got := buildFakeUserMessage(day, plan, rec)
			for _, ph := range []string{"%s", "%d"} {
				if strings.Contains(got, ph) {
					t.Errorf("user-сообщение с сырым плейсхолдером %q:\n%s", ph, got)
				}
			}
		}
	})
}

// TestFakePromptContract — контракт промпта фейк-дайджеста (v4, гибрид «2+2»): статичен (без
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
		{"rubric_marker", strings.Contains(p, "в тематике своей рубрики"),
			"нет маркера «в тематике своей рубрики» — рубрики плана не доходит до LLM"},
		{"no_old_topic_funnel", !strings.Contains(p, "Темы пародии"),
			"вернулась воронка тем «Темы пародии» из фейк-1.0 — подменяет рубрику недели"},
		// v3: комический механизм + few-shot якоря (регрессия «юмора нет совсем»).
		{"comic_mechanism", strings.Contains(p, "комический механизм"),
			"нет секции комического механизма — deadpan без мотора = сухие новости"},
		{"deadpan_not_dry", strings.Contains(p, "«серьёзно» ≠ «сухо»"),
			"нет маркера «серьёзно ≠ сухо» — разрешение на шутку внутри серьёзной подачи"},
		{"funnel_of_boredom_banned", strings.Contains(p, "НЕ смешная новость — брак"),
			"нет критерия брака «правдоподобная, но не смешная» — модель сыплет сухятину"},
		{"fewshot_tone_examples", strings.Contains(p, "Примеры ТОНА"),
			"нет few-shot примеров тона — регистр юмора не показан модели"},
		{"fewshot_marked_used_forever", strings.Contains(p, "использованными навсегда"),
			"примеры не помечены использованными — LLM будет копировать их еженедельно"},
		// v4: гибрид «2+2» — распределение статей по рубрикам задаёт user-сообщение.
		{"rubric_distribution", strings.Contains(p, "статьи 1–2 — якорная рубрика"),
			"нет якоря «статьи 1–2 — якорная рубрика» — LLM не знает, что статьи 1–2 идут в якорь"},
		{"packages_unbound", strings.Contains(p, "Пакеты — без привязки к рубрикам"),
			"нет «Пакеты — без привязки к рубрикам» — пакеты привяжутся к рубрикам плана"},
		{"three_articles_no_fourth", strings.Contains(p, "если статей три, четвёртой не ищи"),
			"нет «если статей три, четвёртой не ищи» — LLM выдумывает четвёртую статью"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.ok {
				t.Error(c.msg)
			}
		})
	}
}

// TestFakePromptFewShotsSanitizeSafe — few-shot примеры тона переживают санитайзер:
// нет МЕТА-маркеров выдуманности, все ссылки из white-списка fakeHosts, end-to-end
// sanitizeFakeBody по блоку примеров не пуст. Если LLM скопирует пример почти дословно,
// блок не будет дропнут.
func TestFakePromptFewShotsSanitizeSafe(t *testing.T) {
	p := prompts.Get(prompts.FakeNews)
	start := strings.Index(p, "Примеры ТОНА")
	end := strings.Index(p, "Задача:")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("нет блока примеров: start=%d end=%d", start, end)
	}
	block := strings.TrimSpace(p[start:end])
	ex := strings.ToLower(block)
	for _, m := range fakeMetaMarkers {
		if strings.Contains(ex, m) {
			t.Errorf("few-shot содержит МЕТА-маркер %q — копия примера будет дропнута санитайзером", m)
		}
	}
	for _, m := range mdLinkRe.FindAllStringSubmatch(block, -1) {
		if !fakeHostAllowed(m[1]) {
			t.Errorf("ссылка примера вне white-списка: %s", m[1])
		}
	}
	if got := sanitizeFakeBody(block); got == "" {
		t.Error("sanitizeFakeBody дропнул весь блок примеров — копия примера не переживёт санитайзер")
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
