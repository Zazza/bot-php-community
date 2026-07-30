package moderation

import (
	"strings"
	"testing"
	"time"
)

// TestHeuristic — stateless-эвристики (invite/реф, CAPS, массовые @-упоминания).
// Для каждого кейса — свежий SpamFilter с nil-зависимостями (Heuristic их не использует).
func TestHeuristic(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantHit bool
	}{
		{"invite_plus_link", "Заходи в группу: t.me/+abc123", true},
		{"invite_joinchat_url", "https://t.me/joinchat/xyz", true},
		{"invite_ref_joinchat", "Пиши в ЛС, скину рефку joinchat/abc", true},
		{"caps_radio", "ПРИВЕТ ВСЕМ PHP РАЗРАБОТЧИКАМ ТУТ", true},
		{"mass_mentions", "@user1 @user2 @user3 всем привет", true},
		{"normal_php_text", "В PHP 8.3 появился readonly-diff, классно", false},
		{"short_ok", "ок", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := NewSpamFilter(nil, nil, nil, nil, 0, SpamConfig{
				FloodMsgs:   10,
				FloodWindow: time.Minute,
			})
			hit, reason := f.Heuristic(SpamInput{UserID: 1, Text: c.text})
			if hit != c.wantHit {
				t.Errorf("Heuristic(text=%q) hit=%v, want %v (reason=%q)", c.text, hit, c.wantHit, reason)
			}
		})
	}
}

// TestHeuristicRepeat — повтор подряд одного и того же сообщения → hit.
// Stateful: bucket хранит последнее сообщение пользователя.
func TestHeuristicRepeat(t *testing.T) {
	f := NewSpamFilter(nil, nil, nil, nil, 0, SpamConfig{
		FloodMsgs:   10,
		FloodWindow: time.Minute,
	})
	const text = "привет всем"
	hit, reason := f.Heuristic(SpamInput{UserID: 1, Text: text})
	if hit {
		t.Fatalf("first Heuristic(%q) hit=%v, want false (reason=%q)", text, hit, reason)
	}
	hit, reason = f.Heuristic(SpamInput{UserID: 1, Text: text})
	if !hit {
		t.Fatalf("second identical Heuristic(%q) hit=false, want true", text)
	}
	if reason != "повтор подряд" {
		t.Errorf("reason=%q, want %q", reason, "повтор подряд")
	}
}

// TestHeuristicFlood — FloodMsgs-е сообщение в окне FloodWindow → hit "флуд".
// Stateful: используем разные тексты, чтобы не сработала эвристика повтора.
func TestHeuristicFlood(t *testing.T) {
	f := NewSpamFilter(nil, nil, nil, nil, 0, SpamConfig{
		FloodMsgs:   3,
		FloodWindow: time.Minute,
	})
	texts := []string{"сообщение раз", "сообщение два", "сообщение три"}
	hits := make([]bool, len(texts))
	var lastReason string
	for i, txt := range texts {
		hit, reason := f.Heuristic(SpamInput{UserID: 1, Text: txt})
		hits[i] = hit
		lastReason = reason
	}
	if hits[0] || hits[1] {
		t.Errorf("first two calls should not hit, got %v", hits[:2])
	}
	if !hits[len(hits)-1] {
		t.Fatalf("call #%d should hit (flood), got false", len(hits))
	}
	if lastReason != "флуд" {
		t.Errorf("last reason=%q, want %q", lastReason, "флуд")
	}
}

// TestParseSpamVerdict — устойчивый парсинг LLM-ответа; fail-safe: мусор → (false, "").
func TestParseSpamVerdict(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantSpam   bool
		wantReason string
	}{
		{"spam_true_with_reason", `{"spam": true, "reason": "реклама"}`, true, "реклама"},
		{"markdown_json_wrapped", "```json\n{\"spam\":true,\"reason\":\"x\"}\n```", true, "x"},
		{"bare_codeblock", "```\n{\"spam\":true,\"reason\":\"y\"}\n```", true, "y"},
		{"spam_false_with_reason", `{"spam": false, "reason": "ok"}`, false, "ok"},
		{"spam_false_no_reason", `{"spam": false}`, false, ""},
		{"invalid_json_garbage", `not a json at all`, false, ""},
		{"empty_string", ``, false, ""},
		{"malformed_braces", `{bad json}`, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotSpam, gotReason := parseSpamVerdict(c.raw)
			if gotSpam != c.wantSpam {
				t.Errorf("parseSpamVerdict(%q) spam=%v, want %v", c.raw, gotSpam, c.wantSpam)
			}
			if gotReason != c.wantReason {
				t.Errorf("parseSpamVerdict(%q) reason=%q, want %q", c.raw, gotReason, c.wantReason)
			}
		})
	}
}

// TestSanitizeReason — invite/реф-паттерны вырезаются из причины перед постом (анти-реляция).
func TestSanitizeReason(t *testing.T) {
	cases := []string{
		"заходи t.me/+abc в группу",
		"https://t.me/joinchat/xyz",
		"пиши joinchat/abc",
		"T.ME/+ABC",
	}
	for _, in := range cases {
		got := sanitizeReason(in)
		if containsAny(strings.ToLower(got), "t.me/+", "t.me/joinchat", "joinchat") {
			t.Errorf("sanitizeReason(%q) = %q: spam-паттерн выжил", in, got)
		}
	}
	if got := sanitizeReason("реклама крипты"); got != "реклама крипты" {
		t.Errorf("sanitizeReason без ссылок изменил текст: %q", got)
	}
}
