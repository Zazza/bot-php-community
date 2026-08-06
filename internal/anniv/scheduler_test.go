package anniv

import (
	"strings"
	"testing"
)

func TestPluralYears(t *testing.T) {
	cases := map[int]string{
		1: "год", 2: "года", 3: "года", 4: "года", 5: "лет",
		11: "лет", 12: "лет", 14: "лет", 21: "год", 22: "года",
		25: "лет", 100: "лет", 101: "год", 111: "лет",
	}
	for n, want := range cases {
		if got := pluralYears(n); got != want {
			t.Errorf("pluralYears(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestLooksLikeHandle(t *testing.T) {
	cases := map[string]bool{
		"dsamotoy":        true,
		"php_voronezh":    true,
		"abc":             false, // <5 символов
		"Ольга":           false, // кириллица
		"name.with.dot":   false, // точка недопустима
		"user name":       false, // пробел
		"this_is_way_too_long_for_a_telegram_handle_x": false, // >32
	}
	for s, want := range cases {
		if got := looksLikeHandle(s); got != want {
			t.Errorf("looksLikeHandle(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	cases := map[string]string{
		"":                "участник",
		"user":            "участник",
		"Ольга Радевская": "Ольга Радевская", // отображаемое имя — без @
		"abc":             "abc",             // слишком короткое для handle — без @
		"dsamotoy":        "@dsamotoy",       // настоящий handle — с @
	}
	for in, want := range cases {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnnounce(t *testing.T) {
	t.Run("with card", func(t *testing.T) {
		got := announce("Ольга", 5, "Тёплый ветеран чата.")
		if !strings.Contains(got, "🎉 **Ольга** уже 5 лет с нами! 🎂") {
			t.Errorf("missing head: %s", got)
		}
		if !strings.Contains(got, "Тёплый ветеран чата.") {
			t.Errorf("missing card: %s", got)
		}
	})
	t.Run("empty card is just head", func(t *testing.T) {
		got := announce("Иван", 2, "")
		want := "🎉 **Иван** уже 2 года с нами! 🎂"
		if got != want {
			t.Errorf("empty card: got %q, want %q", got, want)
		}
	})
	t.Run("whitespace-only card falls back to head", func(t *testing.T) {
		got := announce("X", 1, "   \n  ")
		if strings.Count(got, "\n\n\n") != 0 {
			t.Errorf("blank card should collapse, got: %q", got)
		}
	})
}
