package quiz

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Что выведет: echo '1' + '1a';?", "что выведет echo 1 1a"},
		{"  PHP-8??   type_juggling!! ", "php 8 type juggling"},
		{"Привет, ёж!", "привет ёж"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeKey(c.in); got != c.want {
			t.Errorf("normalizeKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// регистронезависимость + идентичность нормализации для дедупа
	if normalizeKey("Type-Juggling!") != normalizeKey("type juggling") {
		t.Error("normalizeKey should be case/punct-insensitive")
	}
}

func TestParseVerify(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"1", 1},
		{"-1", -1},
		{"  2  ", 2},
		{"Ответ: 0", 0},
		{"3.", 3},
		{"-1.", -1},
		{"непонятно", -1},
		{"", -1},
	}
	for _, c := range cases {
		if got := parseVerify(c.in); got != c.want {
			t.Errorf("parseVerify(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{`вот {"category":"x","correct":2} как-то так`, `{"category":"x","correct":2}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"нет объекта", "{}"},
	}
	for _, c := range cases {
		if got := extractJSONObject(c.in); got != c.want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidQuiz(t *testing.T) {
	opts4 := []string{"a", "b", "c", "d"}
	cases := []struct {
		name string
		q    *quizJSON
		want bool
	}{
		{"valid", &quizJSON{Question: "Q", Options: opts4, Correct: 2}, true},
		{"skip", &quizJSON{Skip: true, Question: "Q", Options: opts4, Correct: 0}, true}, // skip не валидирует структуру
		{"nil", nil, false},
		{"empty question", &quizJSON{Options: opts4, Correct: 0}, false},
		{"three opts", &quizJSON{Question: "Q", Options: []string{"a", "b", "c"}, Correct: 0}, false},
		{"empty opt", &quizJSON{Question: "Q", Options: []string{"a", "b", "c", "  "}, Correct: 0}, false},
		{"correct out of range", &quizJSON{Question: "Q", Options: opts4, Correct: 4}, false},
		{"correct negative", &quizJSON{Question: "Q", Options: opts4, Correct: -1}, false},
	}
	for _, c := range cases {
		if got := validQuiz(c.q); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTrimOpts(t *testing.T) {
	got := trimOpts([]string{"  a ", "b", "c", "d", "e"})
	want := []string{"a", "b", "c", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("trimOpts = %v, want %v", got, want)
	}
}

// TestBuildSeenKeysDedup — регрессия на баг-дубликат: тот же вопрос другой формулировкой
// должен детектиться как повтор (нормализация срезает пунктуацию/регистр/лишние пробелы).
func TestBuildSeenKeysDedup(t *testing.T) {
	recent := []RecentKey{
		{Category: "Type-Juggling", Question: "Что выведет: echo '1' + '1a'; ?"},
		{Category: "psr", Question: "Какой PSR описывает автозагрузчик?"},
	}
	text, cats := buildSeenKeys(recent)

	// перефразированный/другорегистровый тот же вопрос — повтор
	if _, dup := text[normalizeKey("что выведет echo '1' + '1a';?")]; !dup {
		t.Error("rephrased duplicate question not detected")
	}
	// совсем другой вопрос — не повтор
	if _, dup := text[normalizeKey("Какой PSR описывает логер?")]; dup {
		t.Error("different question falsely flagged as duplicate")
	}
	// категория тоже нормализована
	if _, ok := cats["type juggling"]; !ok {
		t.Errorf("category not normalized: %v", cats)
	}
}

func TestRenderQuestion(t *testing.T) {
	got := renderQuestion("Что выведет?", []string{"1", "2", "3", "4"}, 0, 0)
	for _, want := range []string{"🧩 Что выведет?", "A) 1", "D) 4"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Голосов") {
		t.Errorf("tally should be hidden at 0:\n%s", got)
	}

	got2 := renderQuestion("Q", []string{"a", "b", "c", "d"}, 5, 2)
	for _, want := range []string{"Голосов: 5", "Верно: 2"} {
		if !strings.Contains(got2, want) {
			t.Errorf("missing %q in:\n%s", want, got2)
		}
	}
}

func TestRevealToast(t *testing.T) {
	row := &Row{Question: "Q", Opt1: "a", Opt2: "b", Opt3: "c", Opt4: "d", Correct: 1,
		Explanation: "потому что приведение типов"}
	counts := map[int]int{0: 2, 1: 3, 3: 1} // b (idx1) — верный
	got := revealToast(row, counts)
	for _, want := range []string{"Правильно: B) b", "верно 3 из 6", "💡 потому что приведение типов"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}

	// без голосов и без объяснения — компактно
	got0 := revealToast(&Row{Opt1: "a", Opt2: "b", Correct: 0}, map[int]int{})
	if !strings.Contains(got0, "Правильно: A) a") || strings.Contains(got0, "верно") || strings.Contains(got0, "💡") {
		t.Errorf("unexpected bare toast: %s", got0)
	}
}
