package quiz

import (
	"slices"
	"testing"
	"time"
)

func at(day time.Weekday, h, m int) time.Time {
	t0 := time.Date(2026, 8, 1, h, m, 0, 0, time.Local)
	for t0.Weekday() != day {
		t0 = t0.AddDate(0, 0, 1)
	}
	return t0
}

func TestWindowsFor(t *testing.T) {
	oncall := []time.Weekday{time.Wednesday, time.Friday}
	cases := []struct {
		name string
		day  time.Weekday
		cfg  schedCfg
		want []window
	}{
		{"regular day both windows", time.Monday, schedCfg{windows: defaultWindows, onCallDays: oncall}, []window{{10, 13}, {15, 17}}},
		{"oncall wednesday morning only", time.Wednesday, schedCfg{windows: defaultWindows, onCallDays: oncall}, []window{{10, 13}}},
		{"oncall friday morning only", time.Friday, schedCfg{windows: defaultWindows, onCallDays: oncall}, []window{{10, 13}}},
		{"no oncall days all windows", time.Wednesday, schedCfg{windows: defaultWindows}, []window{{10, 13}, {15, 17}}},
		{"empty windows", time.Wednesday, schedCfg{onCallDays: oncall}, nil},
	}
	for _, c := range cases {
		got := windowsFor(c.day, c.cfg)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWindowFor(t *testing.T) {
	cases := []struct {
		name string
		hour int
		want window
		ok   bool
	}{
		{"before all windows", 9, window{}, false},
		{"first window start", 10, window{10, 13}, true},
		{"first window mid", 12, window{10, 13}, true},
		{"first window end exclusive", 13, window{}, false},
		{"gap between windows", 14, window{}, false},
		{"second window start", 15, window{15, 17}, true},
		{"second window mid", 16, window{15, 17}, true},
		{"second window end exclusive", 17, window{}, false},
		{"late evening", 22, window{}, false},
	}
	for _, c := range cases {
		got, ok := windowFor(c.hour, defaultWindows)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestShouldPost(t *testing.T) {
	oncall := schedCfg{silence: 2 * time.Hour, windows: defaultWindows, onCallDays: []time.Weekday{time.Wednesday, time.Friday}}
	noOncall := schedCfg{silence: 2 * time.Hour, windows: defaultWindows}
	cases := []struct {
		name           string
		now            time.Time
		postedInWindow int
		silent         bool
		cfg            schedCfg
		want           bool
	}{
		{"before windows", at(time.Monday, 9, 30), 0, true, oncall, false},
		{"window start silent", at(time.Monday, 10, 0), 0, true, oncall, true},
		{"morning silent", at(time.Monday, 11, 0), 0, true, oncall, true},
		{"morning live no fallback", at(time.Monday, 12, 25), 0, false, oncall, false},
		{"morning live near end no fallback", at(time.Monday, 12, 35), 0, false, oncall, false},
		{"morning live last minute no fallback", at(time.Monday, 12, 59), 0, false, oncall, false},
		{"morning end boundary", at(time.Monday, 13, 0), 0, true, oncall, false},
		{"gap between windows", at(time.Monday, 14, 0), 0, true, oncall, false},
		{"afternoon silent", at(time.Monday, 16, 0), 0, true, oncall, true},
		{"afternoon live no fallback", at(time.Monday, 16, 40), 0, false, oncall, false},
		{"afternoon end boundary", at(time.Monday, 17, 0), 0, true, oncall, false},
		{"already posted in window", at(time.Monday, 11, 0), 1, true, oncall, false},
		{"already posted live", at(time.Monday, 12, 40), 1, false, oncall, false},
		{"oncall wednesday morning", at(time.Wednesday, 11, 0), 0, true, oncall, true},
		{"oncall wednesday afternoon blocked", at(time.Wednesday, 16, 0), 0, true, oncall, false},
		{"oncall friday afternoon blocked", at(time.Friday, 15, 30), 0, true, oncall, false},
		{"no oncall wednesday afternoon", at(time.Wednesday, 16, 0), 0, true, noOncall, true},
	}
	for _, c := range cases {
		got := shouldPost(c.now, c.postedInWindow, c.silent, c.cfg)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDefaultSchedCfg(t *testing.T) {
	if defaultSchedCfg.silence != 2*time.Hour {
		t.Errorf("silence = %v, want 2h", defaultSchedCfg.silence)
	}
	want := []window{{10, 13}, {15, 17}}
	if !slices.Equal(defaultSchedCfg.windows, want) {
		t.Errorf("windows = %v, want %v", defaultSchedCfg.windows, want)
	}
}
