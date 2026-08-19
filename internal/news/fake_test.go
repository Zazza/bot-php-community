package news

import (
	"strings"
	"testing"
	"time"

	"phpbot/internal/md"
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
		in := "**Статьи, которых не было**\n\n[Заголовок](https://php.net/x)\nОписание.\n\n**Пакеты, которых не существует**\n\n• [vrn/php-vedro](https://packagist.org/packages/vrn/php-vedro) — выносит прод в ведро"
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
	t.Run("шапка и дисклеймер всегда на месте", func(t *testing.T) {
		got := assembleFakePost("[Статья](https://php.net/x) текст")
		if !strings.HasPrefix(got, fakeTitle+"\n"+fakeDisclaimer+"\n\n") {
			t.Fatalf("шапка/дисклеймер не в начале: %q", got)
		}
		if !strings.Contains(got, "[Статья](https://php.net/x)") {
			t.Fatalf("тело потеряно: %q", got)
		}
	})
	t.Run("гигантское тело ужато в лимит", func(t *testing.T) {
		block := strings.Repeat("а", 5000) + " [x](https://github.com/x)"
		body := strings.Join([]string{block, block, block}, "\n\n")
		got := assembleFakePost(body)
		if n := md.UTF16Len(got); n > fakePostMaxUTF16 {
			t.Fatalf("длина поста = %d UTF-16, want ≤ %d", n, fakePostMaxUTF16)
		}
		if !strings.Contains(got, fakeDisclaimer) {
			t.Fatalf("дисклеймер потерян при обрезке")
		}
	})
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
