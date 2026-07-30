package messages

import "testing"

func TestNormalizeTopic(t *testing.T) {
	cases := []struct{ in, want string }{
		{"yii3", "yii3"},
		{"Yii 3", "yii3"},
		{"Yii-3", "yii3"},
		{"  YII_3!  ", "yii3"},
		{"PHP 8.2", "php82"},
		{"БД", "бд"},
		{"@user", "user"},
		{"!!!", ""},
		{"x", "x"},
	}
	for _, c := range cases {
		if got := normalizeTopic(c.in); got != c.want {
			t.Fatalf("normalizeTopic(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
