package quiz

import (
	"errors"
	"strings"
	"testing"
)

func TestUTF16Len(t *testing.T) {
	cases := map[string]int{
		"abc": 3,
		"абв": 3,
		"👁":   2, // суррогатная пара
		"👁👁":  4,
	}
	for s, want := range cases {
		if got := utf16Len(s); got != want {
			t.Errorf("utf16Len(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestCapAlert(t *testing.T) {
	t.Run("short unchanged", func(t *testing.T) {
		got := capAlert("короткий текст")
		if got != "короткий текст" {
			t.Errorf("got %q, want unchanged", got)
		}
	})
	t.Run("at limit unchanged", func(t *testing.T) {
		s := strings.Repeat("a", alertMaxUTF16)
		if got := capAlert(s); got != s {
			t.Errorf("строка на границе не должна меняться: got len %d", utf16Len(got))
		}
	})
	t.Run("over limit capped", func(t *testing.T) {
		s := strings.Repeat("a", alertMaxUTF16+100)
		got := capAlert(s)
		if utf16Len(got) > alertMaxUTF16 {
			t.Errorf("capped len %d > %d", utf16Len(got), alertMaxUTF16)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("обрезанный алерт должен заканчиваться «…»: %q", got)
		}
	})
	t.Run("preserves answer prefix", func(t *testing.T) {
		prefix := "Правильно: C) Верный вариант. "
		got := capAlert(prefix + strings.Repeat("я", 300))
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("префикс с верным ответом должен сохраниться: %q", got)
		}
		if utf16Len(got) > alertMaxUTF16 {
			t.Errorf("capped len %d > %d", utf16Len(got), alertMaxUTF16)
		}
	})
	t.Run("emoji counted as 2 utf16", func(t *testing.T) {
		// 200 эмодзи-рун = 400 UTF-16 unit → должен ужаться, а не провалить лимит.
		got := capAlert(strings.Repeat("👁", alertMaxUTF16))
		if utf16Len(got) > alertMaxUTF16 {
			t.Errorf("эмодзи-переполнение: len %d > %d", utf16Len(got), alertMaxUTF16)
		}
	})
}

func TestRevealToastContent(t *testing.T) {
	row := &Row{
		Opt1: "A вариант", Opt2: "B вариант",
		Opt3: "C верный", Opt4: "D вариант",
		Correct: 2, Explanation: "Объяснение тут.",
	}
	got := revealToast(row, nil)
	for _, want := range []string{"Правильно", "C) C верный", "Объяснение тут."} {
		if !strings.Contains(got, want) {
			t.Errorf("revealToast без %q: %q", want, got)
		}
	}
}

// Регрессия бага 2026-08-10: длинное пояснение делало revealToast >200 UTF-16, Telegram
// молча отбрасывал алерт → «Показать ответ» ничего не показывал. capAlert должен ужать,
// сохранив маркер верного ответа и сам текст варианта.
func TestRevealToastCappedFitsLimit(t *testing.T) {
	long := strings.Repeat("подробное пояснение причины ", 40) // ~1.1к символов
	row := &Row{
		Opt1: "A", Opt2: "B", Opt3: "Верный ответ тут", Opt4: "D",
		Correct: 2, Explanation: long,
	}
	got := capAlert(revealToast(row, map[int]int{0: 1, 2: 5, 3: 2}))
	if utf16Len(got) > alertMaxUTF16 {
		t.Errorf("capped reveal len %d > %d: %q", utf16Len(got), alertMaxUTF16, got)
	}
	if !strings.Contains(got, "Правильно") {
		t.Errorf("capped reveal потерял маркер ответа: %q", got)
	}
	if !strings.Contains(got, "Верный ответ тут") {
		t.Errorf("capped reveal потерял текст варианта: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("capped reveal должен обрезать хвост пояснения «…»: %q", got)
	}
}

// Приоритет «почему» над декором: при длинном пояснении тэлли выкидывается первой, а не
// объяснение. Регрессия жалобы «нажимаю Показать ответ — обрезается и не видно почему».
func TestComposeAlertPrioritisesExplanation(t *testing.T) {
	t.Run("длинное пояснение вытесняет тэлли, не наоборот", func(t *testing.T) {
		head := "👁 Правильно: C) public const MY = 'value'"
		ex := strings.Repeat("почему именно так ", 30) // заведомо длиннее остатка бюджета
		tally := " (верно 5 из 8)"
		got := composeAlert(head, strings.TrimSpace(ex), tally)
		if utf16Len(got) > alertMaxUTF16 {
			t.Fatalf("len %d > %d: %q", utf16Len(got), alertMaxUTF16, got)
		}
		if !strings.HasPrefix(got, head) {
			t.Errorf("шапка с верным ответом потеряна: %q", got)
		}
		if !strings.Contains(got, "💡") || !strings.HasSuffix(got, "…") {
			t.Errorf("пояснение должно присутствовать и обрезаться «…»: %q", got)
		}
		if strings.Contains(got, "верно 5 из 8") {
			t.Errorf("декор-тэлли должна уйти под нож раньше пояснения: %q", got)
		}
	})
	t.Run("короткое пояснение — тэлли остаётся", func(t *testing.T) {
		got := composeAlert("👁 Правильно: C) x", "коротко", " (верно 5 из 8)")
		for _, want := range []string{"Правильно: C) x", "💡 коротко", "верно 5 из 8"} {
			if !strings.Contains(got, want) {
				t.Errorf("ждали %q в %q", want, got)
			}
		}
	})
}

// fitRunes не рвёт на полуслове: при обрезке откат до границы слова.
func TestFitRunesWordBoundary(t *testing.T) {
	s := "первое второе третье четвёртое пятое"
	got, cut := fitRunes(s, 20)
	if !cut {
		t.Fatalf("ожидали обрезку для budget=20: %q", got)
	}
	if utf16Len(got) > 20 {
		t.Errorf("len %d > 20: %q", utf16Len(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("должно кончаться «…»: %q", got)
	}
	trimmed := strings.TrimSuffix(got, "…")
	if strings.HasSuffix(trimmed, " ") {
		t.Errorf("хвостовой пробел перед «…» не убран: %q", got)
	}
	// последнее слово не должно быть обрезком: то, что осталось, целиком из исходных слов.
	for _, w := range strings.Fields(trimmed) {
		if !strings.Contains(s, w) {
			t.Errorf("слово %q обрезано на полуслове: %q", w, got)
		}
	}
}

// Регрессия гейта «Показать ответ»: подглядывание верного варианта до собственного ответа
// запрещено. Тексты в таблице — продовые строки revealGate; их смена или перестановка
// проверок (err должен доминировать над voted) ловится здесь. Полную проводку ветки "r"
// в HandleQuizCallback юнит-тестом не покрываем (нужны TG-api и БД).
func TestRevealGate(t *testing.T) {
	cases := []struct {
		name       string
		voted      bool
		err        error
		wantText   string
		wantReveal bool
	}{
		{"db_error_fail_closed", false, errors.New("has ballot: conn refused"), "Ошибка, попробуй ещё", false},
		{"db_error_beats_voted", true, errors.New("has ballot: timeout"), "Ошибка, попробуй ещё", false},
		{"not_voted_blocked", false, nil, "Сначала выбери вариант 🙂", false},
		{"voted_reveals", true, nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, reveal := revealGate(c.voted, c.err)
			if text != c.wantText {
				t.Errorf("revealGate(voted=%v, err=%v) text = %q, want %q", c.voted, c.err, text, c.wantText)
			}
			if reveal != c.wantReveal {
				t.Errorf("revealGate(voted=%v, err=%v) reveal = %v, want %v", c.voted, c.err, reveal, c.wantReveal)
			}
		})
	}
}
