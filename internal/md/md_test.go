package md

import "testing"

func TestToHTML(t *testing.T) {
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
		{"markdown link", "See [docs](https://x.com/a).", "See <a href=\"https://x.com/a\">docs</a>."},
		{"link emoji text", "[🔗](https://x.com)", "<a href=\"https://x.com\">🔗</a>"},
		{"link escapes ampersand in href", "[a](https://x.com/p?q=1&k=2)", "<a href=\"https://x.com/p?q=1&amp;k=2\">a</a>"},
		{"lone bracket not a link", "RFC [для свойств] текст", "RFC [для свойств] текст"},
		{"unclosed link left as-is", "See [docs no close", "See [docs no close"},
		{"bracket then real link", "[x] then [y](https://z.com)", "[x] then <a href=\"https://z.com\">y</a>"},
	}
	for _, c := range cases {
		if got := ToHTML(c.in); got != c.want {
			t.Errorf("%s:\n ToHTML(%q)\n   = %q\n want %q", c.name, c.in, got, c.want)
		}
	}
}
