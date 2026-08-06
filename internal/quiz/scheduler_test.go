package quiz

import (
	"testing"
	"time"
)

// atHour — время сегодня в часе h, минуте m (локальный часовой пояс, как в проде).
func atHour(h, m int) time.Time {
	return time.Date(2026, 8, 6, h, m, 0, 0, time.Local)
}

func TestShouldPost(t *testing.T) {
	cfg := defaultSchedCfg
	longAgo := atHour(6, 0)

	cases := []struct {
		name        string
		now         time.Time
		lastPost    time.Time
		hasPost     bool
		postedToday int
		silent      bool
		want        bool
	}{
		{"cap reached", atHour(14, 0), longAgo, true, 2, true, false},
		{"before window", atHour(9, 30), time.Time{}, false, 0, true, false},
		{"after window", atHour(22, 0), time.Time{}, false, 0, true, false},
		{"silent first of day", atHour(13, 0), time.Time{}, false, 0, true, true},
		{"not silent midday no fallback", atHour(14, 0), time.Time{}, false, 0, false, false},
		{"not silent fallback hour", atHour(21, 30), time.Time{}, false, 0, false, true},
		{"silent second after minInterval", atHour(17, 0), atHour(12, 0), true, 1, true, true},
		{"silent second too soon", atHour(12, 30), atHour(12, 0), true, 1, true, false},
		{"silent at fallback hour posted0", atHour(21, 15), time.Time{}, false, 0, true, true},
		{"fallback but already posted1 not silent", atHour(21, 30), atHour(11, 0), true, 1, false, false},
		{"not silent fallback with yesterday post", atHour(21, 45), longAgo, true, 0, false, true},
	}
	for _, c := range cases {
		got := shouldPost(c.now, c.lastPost, c.hasPost, c.postedToday, c.silent, cfg)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDefaultSchedCfg(t *testing.T) {
	if defaultSchedCfg.dailyCap != 2 {
		t.Errorf("dailyCap = %d, want 2", defaultSchedCfg.dailyCap)
	}
	if defaultSchedCfg.silence != 2*time.Hour || defaultSchedCfg.minInterval != 4*time.Hour {
		t.Errorf("silence/minInterval mismatch: %+v", defaultSchedCfg)
	}
	if !(defaultSchedCfg.windowStart < defaultSchedCfg.fallbackFrom &&
		defaultSchedCfg.fallbackFrom < defaultSchedCfg.windowEnd) {
		t.Errorf("fallback must be inside window: %+v", defaultSchedCfg)
	}
}
