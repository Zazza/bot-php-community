package tg

import (
	"strings"
	"testing"
	"time"
)

func TestParsePeriod(t *testing.T) {
	loc := time.Now().Location()
	cases := []struct {
		name      string
		args      string
		wantErr   bool
		wantSince *time.Time
		wantUntil *time.Time
		sinceNil  bool
		sinceSet  bool
		untilNil  bool
		labelHas  string
	}{
		{
			name:     "empty_all_time",
			args:     "",
			sinceNil: true,
			untilNil: true,
			labelHas: "за всё время",
		},
		{
			name:     "all_keyword",
			args:     "all",
			sinceNil: true,
			untilNil: true,
			labelHas: "за всё время",
		},
		{
			name:     "week_relative",
			args:     "week",
			sinceSet: true,
			untilNil: true,
			labelHas: "за неделю",
		},
		{
			name:     "today_alias",
			args:     "today",
			sinceSet: true,
			untilNil: true,
			labelHas: "за сутки",
		},
		{
			name:      "year_2025",
			args:      "2025",
			wantSince: ptrTime(time.Date(2025, time.January, 1, 0, 0, 0, 0, loc)),
			wantUntil: ptrTime(time.Date(2026, time.January, 1, 0, 0, 0, 0, loc)),
			labelHas:  "2025",
		},
		{
			name:      "month_2025_06",
			args:      "2025-06",
			wantSince: ptrTime(time.Date(2025, time.June, 1, 0, 0, 0, 0, loc)),
			wantUntil: ptrTime(time.Date(2025, time.July, 1, 0, 0, 0, 0, loc)),
			labelHas:  "июн",
		},
		{
			name:    "garbage",
			args:    "garbage",
			wantErr: true,
		},
		{
			name:    "year_out_of_range",
			args:    "1999",
			wantErr: true,
		},
		{
			name:    "bad_month",
			args:    "2025-13",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := parsePeriod(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parsePeriod(%q) err=nil, want error", c.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePeriod(%q) err=%v, want nil", c.args, err)
			}
			switch {
			case c.sinceNil:
				if p.since != nil {
					t.Errorf("since=%v, want nil", *p.since)
				}
			case c.sinceSet:
				if p.since == nil {
					t.Errorf("since=nil, want set")
				}
			case c.wantSince != nil:
				if p.since == nil {
					t.Fatalf("since=nil, want %v", *c.wantSince)
				}
				if !p.since.Equal(*c.wantSince) {
					t.Errorf("since=%v, want %v", *p.since, *c.wantSince)
				}
			}
			switch {
			case c.untilNil:
				if p.until != nil {
					t.Errorf("until=%v, want nil", *p.until)
				}
			case c.wantUntil != nil:
				if p.until == nil {
					t.Fatalf("until=nil, want %v", *c.wantUntil)
				}
				if !p.until.Equal(*c.wantUntil) {
					t.Errorf("until=%v, want %v", *p.until, *c.wantUntil)
				}
			}
			if c.labelHas != "" && !strings.Contains(p.label, c.labelHas) {
				t.Errorf("label=%q, want contains %q", p.label, c.labelHas)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
