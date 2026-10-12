package quiz

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
		{"valid", &quizJSON{Category: "solid", Question: "Q", Options: opts4, Correct: 2}, true},
		{"valid category trimmed", &quizJSON{Category: " clean-code ", Question: "Q", Options: opts4, Correct: 0}, true},
		{"valid category case insensitive", &quizJSON{Category: "Solid", Question: "Q", Options: opts4, Correct: 0}, true},
		{"foreign category", &quizJSON{Category: "type-juggling", Question: "Q", Options: opts4, Correct: 0}, false},
		{"empty category", &quizJSON{Question: "Q", Options: opts4, Correct: 0}, false},
		{"skip", &quizJSON{Skip: true, Category: "solid", Question: "Q", Options: opts4, Correct: 0}, true}, // skip не валидирует структуру
		{"nil", nil, false},
		{"empty question", &quizJSON{Category: "solid", Options: opts4, Correct: 0}, false},
		{"question over limit", &quizJSON{Category: "solid", Question: strings.Repeat("в", maxQuestionBodyRunes+1), Options: opts4, Correct: 0}, false},
		{"option over limit", &quizJSON{Category: "solid", Question: "Q", Options: []string{"a", "b", "c", strings.Repeat("о", maxOptionRunes+1)}, Correct: 0}, false},
		{"explanation over limit", &quizJSON{Category: "solid", Question: "Q", Options: opts4, Correct: 0, Explanation: strings.Repeat("э", maxExplanationRunes+1)}, false},
		{"three opts", &quizJSON{Category: "solid", Question: "Q", Options: []string{"a", "b", "c"}, Correct: 0}, false},
		{"empty opt", &quizJSON{Category: "solid", Question: "Q", Options: []string{"a", "b", "c", "  "}, Correct: 0}, false},
		{"correct out of range", &quizJSON{Category: "solid", Question: "Q", Options: opts4, Correct: 4}, false},
		{"correct negative", &quizJSON{Category: "solid", Question: "Q", Options: opts4, Correct: -1}, false},
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
	text := buildSeenKeys(recent, time.Time{})

	// перефразированный/другорегистровый тот же вопрос — повтор
	if _, dup := text[normalizeKey("что выведет echo '1' + '1a';?")]; !dup {
		t.Error("rephrased duplicate question not detected")
	}
	// совсем другой вопрос — не повтор
	if _, dup := text[normalizeKey("Какой PSR описывает логер?")]; dup {
		t.Error("different question falsely flagged as duplicate")
	}
}

// TestBuildSeenKeysWindow — регрессия «fallback-раунд вырубал текст-дедуп в ноль»:
// окно раунда задаётся через since, и ослабленный второй раунд (7 дней) всё равно видит
// свежие повторы — текст-дедуп не выключается никогда. Запись возрастом 10 дней
// (внутри 30д, старше 7д) дедупится только строгим раундом; 2 дня — обоими; 40 дней — никем.
func TestBuildSeenKeysWindow(t *testing.T) {
	now := time.Now()
	recent := []RecentKey{
		{Category: "сорокадневная", Question: "сорок дней назад", CreatedAt: now.AddDate(0, 0, -40)},
		{Category: "десятидневная", Question: "десять дней назад", CreatedAt: now.AddDate(0, 0, -10)},
		{Category: "свежая", Question: "два дня назад", CreatedAt: now.AddDate(0, 0, -2)},
	}
	cases := []struct {
		name   string
		days   int
		want10 bool
		want2  bool
		want40 bool
	}{
		{"strict_round_30d", dedupDays, true, true, false},
		{"fallback_round_7d", dedupFallbackDays, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := buildSeenKeys(recent, now.AddDate(0, 0, -c.days))
			keys := []struct {
				label string
				q     string
				want  bool
			}{
				{"age_10d_in_30d_out_7d", "десять дней назад", c.want10},
				{"age_2d_in_both", "два дня назад", c.want2},
				{"age_40d_in_neither", "сорок дней назад", c.want40},
			}
			for _, k := range keys {
				_, inText := text[normalizeKey(k.q)]
				if inText != k.want {
					t.Errorf("%s: вопрос %q в text-seen=%v, want %v (окно %d д)", k.label, k.q, inText, k.want, c.days)
				}
			}
		})
	}
}

// TestRecentSince — окно раунда для бан-листа: записи старше since отбрасываются,
// свежие (и только они) остаются; пустой вход → пустой выход.
func TestRecentSince(t *testing.T) {
	now := time.Now()
	recent := []RecentKey{
		{Question: "старый", CreatedAt: now.AddDate(0, 0, -10)},
		{Question: "свежий", CreatedAt: now.AddDate(0, 0, -2)},
	}
	got := recentSince(recent, now.AddDate(0, 0, -7))
	if len(got) != 1 || got[0].Question != "свежий" {
		t.Errorf("recentSince(окно 7д) = %+v, want только [свежий]", got)
	}
	if gotNil := recentSince(nil, now); len(gotNil) != 0 {
		t.Errorf("recentSince(nil) = %+v, want пусто", gotNil)
	}
}

// TestBuildRecentQuestions — бан-лист для генератора: строка «category: текст вопроса»
// (префикс категории — одна категория не маскирует другую; без категории — просто текст),
// не больше max строк, остаются первые по порядку слайса (RecentKeys отдаёт DESC — свежие
// первыми, т.е. хвост из старых отбрасывается), текст вопроса длиннее maxQuestionRunes рун
// обрезан и помечен «…», короткие тексты и пустой вход не искажаются.
func TestBuildRecentQuestions(t *testing.T) {
	long := strings.Repeat("в", maxQuestionRunes+50)
	wantLong := strings.Repeat("в", maxQuestionRunes) + "…"

	tooMany := make([]RecentKey, 0, maxBannedQuestions+5)
	wantCapped := make([]string, 0, maxBannedQuestions)
	for i := 0; i < maxBannedQuestions+5; i++ {
		tooMany = append(tooMany, RecentKey{Question: fmt.Sprintf("вопрос %02d", i)})
		if i < maxBannedQuestions {
			wantCapped = append(wantCapped, fmt.Sprintf("вопрос %02d", i))
		}
	}

	cases := []struct {
		name   string
		recent []RecentKey
		want   []string
	}{
		{"empty", nil, []string{}},
		{"short_untouched", []RecentKey{{Question: "Что ГЛАВНОЕ не так в этом коде?"}}, []string{"Что ГЛАВНОЕ не так в этом коде?"}},
		{"category_prefixed", []RecentKey{{Category: "solid", Question: "Что ГЛАВНОЕ не так?"}}, []string{"solid: Что ГЛАВНОЕ не так?"}},
		{"whitespace_only_skipped", []RecentKey{{Category: "solid", Question: "   "}, {Question: "реальный вопрос"}}, []string{"реальный вопрос"}},
		{"long_truncated", []RecentKey{{Question: long}}, []string{wantLong}},
		{"long_truncated_with_category", []RecentKey{{Category: "clean-code", Question: long}}, []string{"clean-code: " + wantLong}},
		{"capped_at_max", tooMany, wantCapped},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildRecentQuestions(c.recent, maxBannedQuestions)
			if len(got) != len(c.want) {
				t.Fatalf("case %s: got %d строк, want %d", c.name, len(got), len(c.want))
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("case %s:\ngot  %v\nwant %v", c.name, got, c.want)
			}
			// вопрос-часть строки (после префикса «category: ») укладывается в лимит
			for _, q := range got {
				body := q
				if i := strings.Index(q, ": "); i > 0 {
					if _, ok := allowedCategories[strings.ToLower(q[:i])]; ok {
						body = q[i+2:]
					}
				}
				if n := utf8.RuneCountInString(body); n > maxQuestionRunes+1 {
					t.Errorf("case %s: вопрос длиннее лимита: %d рун > %d+1 («…»)", c.name, n, maxQuestionRunes)
				}
			}
		})
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
