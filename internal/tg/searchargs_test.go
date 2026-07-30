package tg

import "testing"

func TestParseSearchArgs(t *testing.T) {
	cases := []struct {
		name      string
		args      string
		wantQuery string
		wantUser  string
		wantSince bool
		wantUntil bool
		wantLabel string
	}{
		{name: "plain", args: "pgvector", wantQuery: "pgvector", wantLabel: "за всё время"},
		{name: "with user", args: "pgvector @ivan", wantQuery: "pgvector", wantUser: "ivan", wantLabel: "за всё время"},
		{name: "week", args: "pgvector week", wantQuery: "pgvector", wantSince: true, wantLabel: "за неделю"},
		{name: "user and month", args: "enum @ivan 2025-06", wantQuery: "enum", wantUser: "ivan", wantSince: true, wantUntil: true, wantLabel: "за июнь 2025"},
		{name: "number not period", args: "php 8", wantQuery: "php 8", wantLabel: "за всё время"},
		{name: "only user", args: "@ivan", wantQuery: "", wantUser: "ivan", wantLabel: "за всё время"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, u, p := parseSearchArgs(tc.args)
			if q != tc.wantQuery {
				t.Errorf("query=%q want %q", q, tc.wantQuery)
			}
			if u != tc.wantUser {
				t.Errorf("username=%q want %q", u, tc.wantUser)
			}
			if (p.since != nil) != tc.wantSince {
				t.Errorf("since present=%v want %v", p.since != nil, tc.wantSince)
			}
			if (p.until != nil) != tc.wantUntil {
				t.Errorf("until present=%v want %v", p.until != nil, tc.wantUntil)
			}
			if p.label != tc.wantLabel {
				t.Errorf("label=%q want %q", p.label, tc.wantLabel)
			}
		})
	}
}
