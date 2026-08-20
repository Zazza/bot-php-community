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
