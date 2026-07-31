package tg

import "testing"

func TestMdToHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain reserved chars stay (HTML-mode safe)", "a.b-c_d>e!f", "a.b-c_d&gt;e!f"},
		{"gt in prose escaped", "Go > PHP", "Go &gt; PHP"},
		{"version dots untouched", "Версия 8.1.2 вышла", "Версия 8.1.2 вышла"},
		{"inline code", "Use `array_filter` here", "Use <code>array_filter</code> here"},
		{"inline code escapes angle brackets", "`array<T>`", "<code>array&lt;T&gt;</code>"},
		{"bold", "**Важно**: сделай", "<b>Важно</b>: сделай"},
		{"fenced block strips lang line", "```php\n$arr = [1, 2];\n```", "<pre><code>$arr = [1, 2];\n</code></pre>"},
		{"fenced block no close left as-is", "```php\ncode", "```php\ncode"},
		{"bold no close left as-is", "**bold", "**bold"},
		{"mixed", "Result: **yes**\n```go\nx := 1\n```\nSee `x`.",
			"Result: <b>yes</b>\n<pre><code>x := 1\n</code></pre>\nSee <code>x</code>."},
	}
	for _, c := range cases {
		if got := mdToHTML(c.in); got != c.want {
			t.Errorf("%s:\n mdToHTML(%q)\n   = %q\n want %q", c.name, c.in, got, c.want)
		}
	}
}
