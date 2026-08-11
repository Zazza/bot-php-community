// Package md — конвертация подмножества markdown (из ответов LLM) в Telegram HTML.
package md

import (
	"html"
	"strings"
)

// ToHTML переводит подмножество markdown в Telegram HTML: fenced ```code```, inline `code`,
// **bold**, [text](url). Весь остальной текст проходит через html.EscapeString, поэтому парс
// не может сломаться на '.', '-', '>', '<' и прочих спецсимволах (главная боль MarkdownV2).
// '[' без полного паттерна ](...) выводится как обычный символ.
func ToHTML(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		fF := strings.Index(s, "```")
		fB := strings.Index(s, "**")
		fI := strings.IndexByte(s, '`')
		fL := strings.IndexByte(s, '[')
		pos := -1
		for _, v := range []int{fF, fB, fI, fL} {
			if v >= 0 && (pos < 0 || v < pos) {
				pos = v
			}
		}
		if pos < 0 {
			b.WriteString(html.EscapeString(s))
			return b.String()
		}
		b.WriteString(html.EscapeString(s[:pos]))
		rest := s[pos:]
		switch {
		case strings.HasPrefix(rest, "```"):
			end := strings.Index(rest[3:], "```")
			if end < 0 {
				b.WriteString(html.EscapeString(rest))
				return b.String()
			}
			content := rest[3 : 3+end]
			if nl := strings.IndexByte(content, '\n'); nl >= 0 {
				content = content[nl+1:] // отбрасываем строку ```lang
			} else {
				content = "" // ```lang``` одной строкой → токен языка без кода
			}
			b.WriteString("<pre><code>")
			b.WriteString(html.EscapeString(content))
			b.WriteString("</code></pre>")
			s = rest[3+end+3:]
		case strings.HasPrefix(rest, "**"):
			end := strings.Index(rest[2:], "**")
			if end < 0 {
				b.WriteString(html.EscapeString(rest))
				return b.String()
			}
			b.WriteString("<b>")
			b.WriteString(html.EscapeString(rest[2 : 2+end]))
			b.WriteString("</b>")
			s = rest[2+end+2:]
		case strings.HasPrefix(rest, "["):
			// markdown-ссылка [text](url) → <a href>. '[', не входящий в полный паттерн
			// (нет ']' или после ']' нет '(', или нет закрывающей ')') — обычный символ.
			closeBracket := strings.IndexByte(rest[1:], ']')
			afterParen := ""
			closeParen := -1
			if closeBracket >= 0 && strings.HasPrefix(rest[1+closeBracket:], "](") {
				afterParen = rest[1+closeBracket+2:]
				closeParen = strings.IndexByte(afterParen, ')')
			}
			if closeParen < 0 {
				b.WriteString("[")
				s = rest[1:]
			} else {
				b.WriteString(`<a href="`)
				b.WriteString(html.EscapeString(afterParen[:closeParen]))
				b.WriteString(`">`)
				b.WriteString(html.EscapeString(rest[1 : 1+closeBracket]))
				b.WriteString("</a>")
				s = afterParen[closeParen+1:]
			}
		default: // одиночный '`' → inline-код
			end := strings.IndexByte(rest[1:], '`')
			if end < 0 {
				b.WriteString(html.EscapeString(rest))
				return b.String()
			}
			b.WriteString("<code>")
			b.WriteString(html.EscapeString(rest[1 : 1+end]))
			b.WriteString("</code>")
			s = rest[1+end+1:]
		}
	}
	return b.String()
}
