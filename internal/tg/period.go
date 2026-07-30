package tg

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type period struct {
	since *time.Time
	until *time.Time
	label string
}

var russianMonths = map[int]string{
	1:  "январь",
	2:  "февраль",
	3:  "март",
	4:  "апрель",
	5:  "май",
	6:  "июнь",
	7:  "июль",
	8:  "август",
	9:  "сентябрь",
	10: "октябрь",
	11: "ноябрь",
	12: "декабрь",
}

func parsePeriod(args string) (period, error) {
	a := strings.ToLower(strings.TrimSpace(args))
	loc := time.Now().Location()
	now := time.Now()
	switch a {
	case "", "all":
		return period{label: "за всё время"}, nil
	case "day", "today":
		since := now.Add(-24 * time.Hour)
		return period{since: &since, label: "за сутки"}, nil
	case "week":
		since := now.Add(-7 * 24 * time.Hour)
		return period{since: &since, label: "за неделю"}, nil
	case "month":
		since := now.AddDate(0, -1, 0)
		return period{since: &since, label: "за месяц"}, nil
	}
	if y, ok := parseYear(a); ok {
		since := time.Date(y, time.January, 1, 0, 0, 0, 0, loc)
		until := since.AddDate(1, 0, 0)
		return period{since: &since, until: &until, label: fmt.Sprintf("за %d год", y)}, nil
	}
	if y, m, ok := parseYearMonth(a); ok {
		since := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, loc)
		until := since.AddDate(0, 1, 0)
		return period{since: &since, until: &until, label: fmt.Sprintf("за %s %d", russianMonths[m], y)}, nil
	}
	return period{}, fmt.Errorf("неизвестный период: %q", args)
}

func parseYear(s string) (int, bool) {
	if len(s) != 4 {
		return 0, false
	}
	y, err := strconv.Atoi(s)
	if err != nil || y < 2000 || y > 2100 {
		return 0, false
	}
	return y, true
}

func parseYearMonth(s string) (year, month int, ok bool) {
	if len(s) != 7 || s[4] != '-' {
		return 0, 0, false
	}
	y, err1 := strconv.Atoi(s[:4])
	m, err2 := strconv.Atoi(s[5:])
	if err1 != nil || err2 != nil || y < 2000 || y > 2100 || m < 1 || m > 12 {
		return 0, 0, false
	}
	return y, m, true
}
