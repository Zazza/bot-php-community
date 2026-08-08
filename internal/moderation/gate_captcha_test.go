package moderation

import (
	"strconv"
	"strings"
	"testing"
)

// TestGenCaptcha — 4 уникальных варианта, верный индекс указывает на реальный ответ выражения.
func TestGenCaptcha(t *testing.T) {
	for i := 0; i < 200; i++ {
		expr, opts, correct := genCaptcha()
		if len(opts) != 4 {
			t.Fatalf("expected 4 options, got %d (%q)", len(opts), opts)
		}
		if correct < 0 || correct > 3 {
			t.Fatalf("correct index out of range: %d", correct)
		}
		seen := make(map[int]bool, 4)
		for _, o := range opts {
			n, err := strconv.Atoi(o)
			if err != nil {
				t.Fatalf("non-numeric option %q", o)
			}
			if n < 0 {
				t.Fatalf("negative option %d", n)
			}
			if seen[n] {
				t.Fatalf("duplicate option %d in %v", n, opts)
			}
			seen[n] = true
		}
		// Проверяем арифметику выражения.
		ns := strings.Fields(expr) // ["7", "+", "3"] | ["9", "−", "4"]
		if len(ns) != 3 {
			t.Fatalf("unexpected expr %q", expr)
		}
		a, errA := strconv.Atoi(ns[0])
		b, errB := strconv.Atoi(ns[2])
		if errA != nil || errB != nil {
			t.Fatalf("parse operands %q: %v %v", expr, errA, errB)
		}
		var want int
		switch ns[1] {
		case "+":
			want = a + b
		case "−", "-":
			want = a - b
		default:
			t.Fatalf("unknown operator %q in %q", ns[1], expr)
		}
		got, _ := strconv.Atoi(opts[correct])
		if got != want {
			t.Fatalf("expr=%q: correct option=%d, want=%d", expr, got, want)
		}
	}
}

// TestGateClickAllowed — регрессия: капчу новичка может решить ТОЛЬКО сам новичок.
// Раньше HandleGateCallback принимал верный ответ от любого участника → сторонний
// человек (или подельник спамера) кликал верный вариант и разматчивал аккаунт.
func TestGateClickAllowed(t *testing.T) {
	newbie := &GateRecord{TGUserID: 4242}
	cases := []struct {
		name      string
		gate      *GateRecord
		clickerID int64
		want      bool
	}{
		{"newbie_himself", newbie, 4242, true},
		{"bystander_blocked", newbie, 9999, false},
		{"zero_clicker_blocked", newbie, 0, false},
		{"nil_gate_blocked", nil, 4242, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gateClickAllowed(c.gate, c.clickerID); got != c.want {
				t.Errorf("gateClickAllowed(clicker=%d) = %v, want %v", c.clickerID, got, c.want)
			}
		})
	}
}
