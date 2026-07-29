package moderation

import "testing"

// TestParseVerdict — проверка устойчивого парсинга ответа судьи.
// Реальный end-to-end с LLM — это integration-тест (требует PHPBOT_LLM_API_KEY),
// здесь покрываем только парсер, который наиболее подвержен регрессиям (markdown-обёртки и т.п.).
func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain_json_bot", `{"verdict":"bot","reason":"spam"}`, "bot"},
		{"plain_json_human", `{"verdict":"human","reason":"real php dev"}`, "human"},
		{"plain_json_unclear", `{"verdict":"unclear","reason":"short"}`, "unclear"},
		{"markdown_wrapped", "```json\n{\"verdict\":\"bot\",\"reason\":\"x\"}\n```", "bot"},
		{"bare_codeblock", "```\n{\"verdict\":\"human\",\"reason\":\"x\"}\n```", "human"},
		{"with_leading_spaces", "  {\"verdict\":\"human\",\"reason\":\"ok\"}  ", "human"},
		{"invalid_json", `not a json`, "unclear"},        // fail-safe
		{"unknown_verdict", `{"verdict":"alien"}`, "unclear"}, // fail-safe
		{"missing_field", `{"reason":"x"}`, "unclear"},   // fail-safe
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseVerdict(c.raw)
			if got.Verdict != c.want {
				t.Errorf("parseVerdict(%q) = %q, want %q (reason=%q)", c.raw, got.Verdict, c.want, got.Reason)
			}
		})
	}
}

// TestVerdictEmoji — тривиальная проверка emoji-маппинга для постов в чат.
func TestVerdictEmoji(t *testing.T) {
	cases := map[string]string{
		"bot":     "🤖",
		"human":   "✅",
		"unclear": "❓",
		"":        "•",
	}
	for v, want := range cases {
		if got := VerdictEmoji(v); got != want {
			t.Errorf("VerdictEmoji(%q) = %q, want %q", v, got, want)
		}
	}
}
