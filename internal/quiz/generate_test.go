package quiz

import (
	"reflect"
	"strings"
	"testing"
)

func TestTopDistinct(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"a", "b", "a", "c", "user", "", "b"}, []string{"a", "b", "c"}},
		{[]string{"x", "x", "x"}, []string{"x"}},
		{[]string{}, []string{}},
		{[]string{"", "user", "  "}, []string{}},
	}
	for _, c := range cases {
		got := topDistinct(c.in, 4)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("topDistinct(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	// лимит n
	if got := topDistinct([]string{"a", "b", "c", "d", "e"}, 3); len(got) != 3 {
		t.Errorf("limit: got %v", got)
	}
}

func TestNewQuestionShuffleKeepsCorrect(t *testing.T) {
	opts := []string{"alice", "bob", "carol", "dave"}
	for i := 0; i < 20; i++ {
		q := newQuestion("k", "p", opts, 0) // верный — alice
		if q.Opts[q.Correct] != "alice" {
			t.Fatalf("correct index wrong: opts=%v correct=%d", q.Opts, q.Correct)
		}
		if len(q.Opts) != 4 {
			t.Fatalf("opts len: %d", len(q.Opts))
		}
		seen := map[string]int{}
		for _, o := range q.Opts {
			seen[o]++
		}
		if seen["alice"] != 1 {
			t.Fatalf("alice not unique: %v", q.Opts)
		}
	}
}

func TestExtractJSONArray(t *testing.T) {
	cases := []struct{ in, want string }{
		{`вот ["Yii4","PHP 9"] список`, `["Yii4","PHP 9"]`},
		{`[1,2,3]`, `[1,2,3]`},
		{`нет массива`, `[]`},
	}
	for _, c := range cases {
		if got := extractJSONArray(c.in); got != c.want {
			t.Errorf("extractJSONArray(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderQuestion(t *testing.T) {
	got := renderQuestion("Кто?", []string{"Да", "Нет", "", ""}, 0, 0)
	for _, want := range []string{"🧩 Кто?", "A) Да", "B) Нет"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Голосов") {
		t.Errorf("tally should be hidden at 0:\n%s", got)
	}

	got2 := renderQuestion("Кто?", []string{"Да", "Нет", "", ""}, 5, 2)
	for _, want := range []string{"Голосов: 5", "Верно: 2"} {
		if !strings.Contains(got2, want) {
			t.Errorf("missing %q in:\n%s", want, got2)
		}
	}

	got3 := renderQuestion("Q", []string{"a", "b", "c", "d"}, 0, 0)
	for _, want := range []string{"A) a", "B) b", "C) c", "D) d"} {
		if !strings.Contains(got3, want) {
			t.Errorf("missing %q in:\n%s", want, got3)
		}
	}
}

func TestRenderReveal(t *testing.T) {
	row := &Row{Question: "Кто?", Opt1: "alice", Opt2: "bob", Opt3: "carol", Opt4: "dave", Correct: 1}
	counts := map[int]int{0: 2, 1: 3, 3: 1} // bob (idx1) — верный
	got := renderReveal(row, counts)
	for _, want := range []string{
		"A) alice — 2",
		"B) bob — 3 ✅",
		"C) carol — 0",
		"D) dave — 1",
		"Правильно: B) bob",
		"Ответили: 6 · Верно: 3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
