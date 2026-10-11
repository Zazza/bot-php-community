package oncall

import (
	"strings"
	"testing"
)

func TestSanitizeLLMText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain_passthrough", "Отвал основной БД", "Отвал основной БД"},
		{"markdown_link_neutralised", "читай [доку](javascript:alert(1)) скорее", "читай (доку)(javascript:alert(1)) скорее"},
		{"lone_open_bracket", "метрики [внешний grafana", "метрики (внешний grafana"},
		{"tag_like_brackets", "[блог] новость", "(блог) новость"},
		{"emoji_untouched", "🚨 SEV1 💬 дежурный 👁", "🚨 SEV1 💬 дежурный 👁"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeLLMText(c.in); got != c.want {
				t.Errorf("%s: sanitizeLLMText(%q) = %q, want %q", c.name, c.in, got, c.want)
			}
		})
	}
	t.Run("invariant_no_square_brackets", func(t *testing.T) {
		got := sanitizeLLMText("смета [meta] & «метрики» 👁 x[1]² и ] просто скобка")
		if strings.ContainsAny(got, "[]") {
			t.Errorf("инвариант нарушен: санитизированный текст содержит квадратные скобки: %q", got)
		}
	})
}

func TestValidSeverity(t *testing.T) {
	cases := []struct {
		name string
		sev  string
		want bool
	}{
		{"sev1", sev1, true},
		{"sev2", sev2, true},
		{"sev3", sev3, true},
		{"empty", "", false},
		{"sev4_unknown", "sev4", false},
		{"case_sensitive", "SEV1", false},
		{"verdict_is_not_severity", verdictClosed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validSeverity(c.sev); got != c.want {
				t.Errorf("%s: validSeverity(%q) = %v, want %v", c.name, c.sev, got, c.want)
			}
		})
	}
}

func TestValidVerdict(t *testing.T) {
	cases := []struct {
		name string
		v    string
		want bool
	}{
		{"closed", verdictClosed, true},
		{"partial", verdictPartial, true},
		{"failed", verdictFailed, true},
		{"empty", "", false},
		{"success_unknown", "success", false},
		{"severity_is_not_verdict", sev1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validVerdict(c.v); got != c.want {
				t.Errorf("%s: validVerdict(%q) = %v, want %v", c.name, c.v, got, c.want)
			}
		})
	}
}

func TestOrDash(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "—"},
		{"spaces", "   ", "—"},
		{"tabs_newlines", "\t\n", "—"},
		{"text_passthrough", "план", "план"},
		{"untrimmed_kept", " x ", " x "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := orDash(c.in); got != c.want {
				t.Errorf("%s: orDash(%q) = %q, want %q", c.name, c.in, got, c.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter_unchanged", "abc", 5, "abc"},
		{"exact_max_unchanged", "abcde", 5, "abcde"},
		{"over_cut_with_ellipsis", "abcdef", 5, "abcde…"},
		{"cyrillic_by_runes_not_bytes", "абвгд", 3, "абв…"},
		{"emoji_is_one_rune", "👁👁👁", 2, "👁👁…"},
		{"empty", "", 3, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := truncateRunes(c.in, c.max); got != c.want {
				t.Errorf("%s: truncateRunes(%q, %d) = %q, want %q", c.name, c.in, c.max, got, c.want)
			}
		})
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain_json", `{"a":1}`, `{"a":1}`},
		{"text_around", `вот {"severity":"sev1"} ок`, `{"severity":"sev1"}`},
		{"markdown_fenced", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"no_object", "нет объекта", "{}"},
		{"unclosed_object", `{"a":1`, "{}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractJSONObject(c.in); got != c.want {
				t.Errorf("%s: extractJSONObject(%q) = %q, want %q", c.name, c.in, got, c.want)
			}
		})
	}
}

func TestParseScenario(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantTitle string
		wantSev   string
		wantNarr  string
	}{
		{"valid_json", `{"title":"Отвал БД","severity":"sev1","narrative":"всё упало","metrics":"err 500"}`, "Отвал БД", "sev1", "всё упало"},
		{"broken_json_plain_text", `не json вообще`, "", "", `не json вообще`},
		{"empty_string", "", "", "", ""},
		{"empty_narrative_falls_back_to_raw", `{"title":"T"}`, "", "", `{"title":"T"}`},
		{"whitespace_narrative_falls_back", `{"narrative":"   "}`, "", "", `{"narrative":"   "}`},
		{"array_falls_back", `[1,2]`, "", "", `[1,2]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := parseScenario(c.raw)
			if sc == nil {
				t.Fatalf("%s: parseScenario(%q) = nil", c.name, c.raw)
			}
			if sc.Title != c.wantTitle {
				t.Errorf("%s: Title = %q, want %q", c.name, sc.Title, c.wantTitle)
			}
			if sc.Severity != c.wantSev {
				t.Errorf("%s: Severity = %q, want %q", c.name, sc.Severity, c.wantSev)
			}
			if sc.Narrative != c.wantNarr {
				t.Errorf("%s: Narrative = %q, want %q", c.name, sc.Narrative, c.wantNarr)
			}
		})
	}
}

func TestParseOutcome(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantTxt string
		wantCom string
		wantSt  string
	}{
		{"valid_json", `{"outcome":"стало лучше","complication":"c","state":"m"}`, "стало лучше", "c", "m"},
		{"broken_plain_text", "просто текст развязки", "просто текст развязки", "", ""},
		{"empty_text_falls_back_to_raw", `{"state":"m"}`, `{"state":"m"}`, "", ""},
		{"empty_string", "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := parseOutcome(c.raw)
			if out == nil {
				t.Fatalf("%s: parseOutcome(%q) = nil", c.name, c.raw)
			}
			if out.Text != c.wantTxt {
				t.Errorf("%s: Text = %q, want %q", c.name, out.Text, c.wantTxt)
			}
			if out.Complication != c.wantCom {
				t.Errorf("%s: Complication = %q, want %q", c.name, out.Complication, c.wantCom)
			}
			if out.State != c.wantSt {
				t.Errorf("%s: State = %q, want %q", c.name, out.State, c.wantSt)
			}
		})
	}
}
