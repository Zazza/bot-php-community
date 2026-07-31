package tg

import "testing"

func TestPluralYears(t *testing.T) {
	cases := map[int]string{
		1: "год", 2: "года", 5: "лет", 11: "лет",
		21: "год", 22: "года", 25: "лет", 101: "год", 111: "лет",
	}
	for n, want := range cases {
		if got := pluralYears(n); got != want {
			t.Errorf("pluralYears(%d) = %q, want %q", n, got, want)
		}
	}
}
