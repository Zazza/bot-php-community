package oncall

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func atTime(h, m, s int) time.Time {
	return time.Date(2026, 10, 7, h, m, s, 0, time.Local)
}

func atWeekday(day time.Weekday, h, m int) time.Time {
	t0 := time.Date(2026, 10, 1, h, m, 0, 0, time.Local)
	for t0.Weekday() != day {
		t0 = t0.AddDate(0, 0, 1)
	}
	return t0
}

func TestPickPlan(t *testing.T) {
	cases := []struct {
		name   string
		counts map[int]int
		nPlans int
		want   int
	}{
		{"no_votes_conservative", map[int]int{}, 3, 0},
		{"nil_counts_conservative", nil, 3, 0},
		{"single_vote_wins", map[int]int{2: 1}, 3, 2},
		{"majority_wins", map[int]int{0: 1, 1: 4, 2: 2}, 3, 1},
		{"distribution_last_plan_wins", map[int]int{0: 1, 1: 2, 2: 3}, 3, 2},
		{"tie_keeps_conservative", map[int]int{0: 2, 1: 2, 2: 1}, 3, 0},
		{"tie_above_zero_keeps_earlier", map[int]int{1: 2, 2: 2}, 3, 1},
		{"out_of_range_indexes_ignored", map[int]int{1: 1, 7: 99, -1: 50}, 3, 1},
		{"zero_plans_returns_zero", map[int]int{0: 5}, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickPlan(c.counts, c.nPlans); got != c.want {
				t.Errorf("%s: pickPlan(%v, %d) = %d, want %d", c.name, c.counts, c.nPlans, got, c.want)
			}
		})
	}
}

func TestInStartWindow(t *testing.T) {
	cases := []struct {
		name      string
		h, m, s   int
		want      bool
	}{
		{"window_start_19_00", 19, 0, 0, true},
		{"inside_19_15", 19, 15, 0, true},
		{"last_minute_19_29", 19, 29, 0, true},
		{"last_second_19_29_59", 19, 29, 59, true},
		{"window_end_19_30_exclusive", 19, 30, 0, false},
		{"before_window_18_59", 18, 59, 59, false},
		{"after_window_20_00", 20, 0, 0, false},
		{"midnight", 0, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := atTime(c.h, c.m, c.s)
			if got := inStartWindow(now); got != c.want {
				t.Errorf("%s: inStartWindow(%s) = %v, want %v", c.name, now.Format("15:04:05"), got, c.want)
			}
		})
	}
}

func TestIsOnCallDay(t *testing.T) {
	days := []time.Weekday{time.Wednesday, time.Friday}
	cases := []struct {
		name string
		days []time.Weekday
		day  time.Weekday
		want bool
	}{
		{"wednesday_on", days, time.Wednesday, true},
		{"friday_on", days, time.Friday, true},
		{"tuesday_off", days, time.Tuesday, false},
		{"thursday_off", days, time.Thursday, false},
		{"saturday_off", days, time.Saturday, false},
		{"sunday_off", days, time.Sunday, false},
		{"nil_days_never", nil, time.Wednesday, false},
		{"empty_days_never", []time.Weekday{}, time.Sunday, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isOnCallDay(c.days, atWeekday(c.day, 12, 0)); got != c.want {
				t.Errorf("%s: isOnCallDay(%v, %v) = %v, want %v", c.name, c.days, c.day, got, c.want)
			}
		})
	}
}

func TestRoundBriefs(t *testing.T) {
	plansJSON := []byte(`[{"title":"Наблюдение","summary":"ждём"},{"title":"Откат","summary":"назад"}]`)
	outcomeJSON := `{"outcome":"стало лучше","complication":"осложнение","state":"метрики"}`
	cases := []struct {
		name        string
		row         RoundRow
		wantPlan    string
		wantOutcome string
	}{
		{"chosen_plan_title", RoundRow{Round: 1, Title: "Отвал БД", Plans: plansJSON, ChosenPlan: 1, Outcome: outcomeJSON}, "Откат", "стало лучше"},
		{"chosen_negative_dash", RoundRow{Round: 2, Title: "T", Plans: plansJSON, ChosenPlan: -1}, "—", ""},
		{"chosen_out_of_range_dash", RoundRow{Round: 1, Plans: plansJSON, ChosenPlan: 9}, "—", ""},
		{"no_plans_dash", RoundRow{Round: 1, Plans: []byte(`битый json`), ChosenPlan: 0}, "—", ""},
		{"broken_outcome_plain_text", RoundRow{Round: 3, Plans: plansJSON, ChosenPlan: 0, Outcome: "просто текст"}, "Наблюдение", "просто текст"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := roundBriefs([]RoundRow{c.row})
			if len(got) != 1 {
				t.Fatalf("%s: got %d брифов, want 1", c.name, len(got))
			}
			if got[0].Plan != c.wantPlan {
				t.Errorf("%s: Plan = %q, want %q", c.name, got[0].Plan, c.wantPlan)
			}
			if got[0].Outcome != c.wantOutcome {
				t.Errorf("%s: Outcome = %q, want %q", c.name, got[0].Outcome, c.wantOutcome)
			}
			if got[0].Round != c.row.Round || got[0].Title != c.row.Title {
				t.Errorf("%s: Round/Title = %d/%q, want %d/%q", c.name, got[0].Round, got[0].Title, c.row.Round, c.row.Title)
			}
		})
	}
	t.Run("empty_rounds", func(t *testing.T) {
		if got := roundBriefs(nil); len(got) != 0 {
			t.Errorf("roundBriefs(nil) = %+v, want пусто", got)
		}
	})
	t.Run("long_outcome_truncated", func(t *testing.T) {
		raw, err := json.Marshal(Outcome{Text: strings.Repeat("х", 400)})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got := roundBriefs([]RoundRow{{Round: 1, Plans: plansJSON, ChosenPlan: 0, Outcome: string(raw)}})
		if n := utf8.RuneCountInString(got[0].Outcome); n != 301 {
			t.Errorf("outcome = %d рун, want 300+«…»=301", n)
		}
		if !strings.HasSuffix(got[0].Outcome, "…") {
			t.Errorf("обрезанный outcome должен кончаться «…»: %q", got[0].Outcome)
		}
	})
}

func TestFallbackOutcome(t *testing.T) {
	cases := []struct{ name, state string }{
		{"with_state", "ошибки 5xx: 12%"},
		{"empty_state", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := fallbackOutcome(c.state)
			if out.Text == "" || out.Complication == "" {
				t.Errorf("%s: fallback развязка пустая: %+v", c.name, out)
			}
			if out.State != c.state {
				t.Errorf("%s: State = %q, want исходное %q (состояние не ломаем)", c.name, out.State, c.state)
			}
		})
	}
}

func TestStaticPostmortem(t *testing.T) {
	pm := staticPostmortem()
	if !validVerdict(pm.Verdict) {
		t.Errorf("verdict = %q, want валидный из enum closed|partial|failed", pm.Verdict)
	}
	if pm.Summary == "" {
		t.Error("Summary пустой — финальный пост без текста")
	}
}

func TestConservativePlan(t *testing.T) {
	if strings.TrimSpace(conservativePlan.Title) == "" {
		t.Error("conservativePlan.Title пустой — авто-дежурный без названия")
	}
	if strings.TrimSpace(conservativePlan.Summary) == "" {
		t.Error("conservativePlan.Summary пустой — авто-дежурный без описания")
	}
}
