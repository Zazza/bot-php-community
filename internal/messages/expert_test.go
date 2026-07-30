package messages

import "testing"

func TestEscapeILIKE(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"yii3", "yii3"},
		{"", ""},
		{"100%ready", `100\%ready`},
		{"user_name", `user\_name`},
		{`path\to`, `path\\to`},
		{"a_b%c", `a\_b\%c`},
	}
	for _, c := range cases {
		if got := escapeILIKE(c.in); got != c.want {
			t.Fatalf("escapeILIKE(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
