package topics

import (
	"strings"
	"testing"
	"time"

	"phpbot/internal/messages"
)

func TestPastWeekCandidates(t *testing.T) {
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	cands := pastWeekCandidates(start, end, pastYearsMax)
	if len(cands) != pastYearsMax {
		t.Fatalf("кандидатов = %d, want %d", len(cands), pastYearsMax)
	}
	for i, c := range cands {
		if c.YearsBack != i+1 {
			t.Errorf("cands[%d].YearsBack = %d, want %d", i, c.YearsBack, i+1)
		}
		if got := c.End.Sub(c.Start); got != end.Sub(start) {
			t.Errorf("cands[%d]: ширина окна = %v, want %v", i, got, end.Sub(start))
		}
		if c.Start.Year() != start.Year()-c.YearsBack {
			t.Errorf("cands[%d].Start.Year() = %d, want %d", i, c.Start.Year(), start.Year()-c.YearsBack)
		}
	}
}

// TestPastWeekCandidatesLeap — окно, содержащее 29 февраля: ширина не должна искажаться
// нормализацией AddDate (AddDate обоих бортов срезал бы сутки).
func TestPastWeekCandidatesLeap(t *testing.T) {
	start := time.Date(2024, 2, 26, 0, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	cands := pastWeekCandidates(start, end, 1)
	wantStart := time.Date(2023, 2, 26, 0, 0, 0, 0, time.UTC)
	wantEnd := wantStart.Add(7 * 24 * time.Hour)
	if !cands[0].Start.Equal(wantStart) || !cands[0].End.Equal(wantEnd) {
		t.Fatalf("leap окно = [%v, %v), want [%v, %v)", cands[0].Start, cands[0].End, wantStart, wantEnd)
	}
}

func TestPickPastWeek(t *testing.T) {
	cases := []struct {
		name  string
		cands []pastWeek
		min   int
		want  int // YearsBack победителя; 0 = nil
	}{
		{"макс побеждает", []pastWeek{{1, time.Time{}, time.Time{}, 10}, {2, time.Time{}, time.Time{}, 50}, {3, time.Time{}, time.Time{}, 20}}, 3, 2},
		{"равный счёт — свежий год", []pastWeek{{1, time.Time{}, time.Time{}, 10}, {2, time.Time{}, time.Time{}, 10}}, 3, 1},
		{"все ниже минимума", []pastWeek{{1, time.Time{}, time.Time{}, 1}, {2, time.Time{}, time.Time{}, 2}}, 3, 0},
		{"пустой срез", nil, 3, 0},
		{"часть ниже минимума", []pastWeek{{1, time.Time{}, time.Time{}, 0}, {2, time.Time{}, time.Time{}, 4}}, 3, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pickPastWeek(c.cands, c.min)
			if c.want == 0 {
				if got != nil {
					t.Fatalf("pickPastWeek = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("pickPastWeek = nil, want YearsBack=%d", c.want)
			}
			if got.YearsBack != c.want {
				t.Fatalf("YearsBack = %d, want %d", got.YearsBack, c.want)
			}
		})
	}
}

func TestYearsAgoLabel(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{1, "Год назад"},
		{2, "2 года назад"},
		{3, "3 года назад"},
		{4, "4 года назад"},
		{5, "5 лет назад"},
	}
	for _, c := range cases {
		if got := yearsAgoLabel(c.n); got != c.want {
			t.Errorf("yearsAgoLabel(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestPastSectionHeader(t *testing.T) {
	cases := []struct {
		w    pastWeek
		want string
	}{
		{pastWeek{YearsBack: 1, Start: time.Date(2025, 8, 18, 0, 0, 0, 0, time.UTC)}, "🕰 Год назад, в августе 2025, обсуждали:"},
		{pastWeek{YearsBack: 3, Start: time.Date(2023, 12, 28, 0, 0, 0, 0, time.UTC)}, "🕰 3 года назад, в декабре 2023, обсуждали:"},
		{pastWeek{YearsBack: 5, Start: time.Date(2021, 2, 22, 0, 0, 0, 0, time.UTC)}, "🕰 5 лет назад, в феврале 2021, обсуждали:"},
	}
	for _, c := range cases {
		if got := pastSectionHeader(c.w); got != c.want {
			t.Errorf("pastSectionHeader(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
}

func TestFormatPastForDigest(t *testing.T) {
	ts := time.Date(2025, 8, 20, 12, 0, 0, 0, time.UTC)

	t.Run("пустой ввод", func(t *testing.T) {
		if got := formatPastForDigest(nil, pastMaxChars); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("кап на одно сообщение", func(t *testing.T) {
		// Кап байтовый (как в chatFormatForDigest): кириллица = 2 байта → 500 байт = 250 «а».
		long := strings.Repeat("а", 600)
		got := formatPastForDigest([]messages.Message{{TS: ts, Username: "u", Text: long}}, pastMaxChars)
		if n := strings.Count(got, "а"); n != pastMaxMsgLen/2 {
			t.Fatalf("кириллических символов после капа = %d, want %d (байтовый кап %d)", n, pastMaxMsgLen/2, pastMaxMsgLen)
		}
		if !strings.HasSuffix(got, "…\n") {
			t.Fatalf("нет «…» после обрезки: %q", got[len(got)-10:])
		}
	})

	t.Run("бюджет: строки целиком, хвост отбрасывается", func(t *testing.T) {
		msgs := []messages.Message{
			{TS: ts, Username: "a", Text: strings.Repeat("x", 50)},
			{TS: ts, Username: "b", Text: strings.Repeat("y", 50)},
			{TS: ts, Username: "c", Text: strings.Repeat("z", 50)},
		}
		got := formatPastForDigest(msgs, 100) // влезает ~1 строка (~70 симв.)
		if !strings.Contains(got, "a: xxx") {
			t.Fatalf("первая строка потеряна: %q", got)
		}
		if strings.Contains(got, "c:") {
			t.Fatalf("третья строка не должна влезть: %q", got)
		}
		if !strings.Contains(got, "…(обрезано)") {
			t.Fatalf("нет маркера обрезки: %q", got)
		}
	})

	t.Run("бюджет больше материала — без маркера обрезки", func(t *testing.T) {
		msgs := []messages.Message{{TS: ts, Username: "a", Text: "привет"}}
		got := formatPastForDigest(msgs, pastMaxChars)
		if strings.Contains(got, "…(обрезано)") {
			t.Fatalf("ложный маркер обрезки: %q", got)
		}
		if !strings.HasPrefix(got, "[20.08 12:00] a: привет\n") {
			t.Fatalf("формат строки: %q", got)
		}
	})
}

func TestAppendRetro(t *testing.T) {
	post := "📋 Дайджест недели\n\nитог"
	cases := []struct {
		name      string
		post      string
		retro     string
		wantRetro bool
	}{
		{name: "влезает", post: post, retro: "🕰 Год назад, в августе 2025, обсуждали:\n- пункт", wantRetro: true},
		{name: "ретро пустое", post: post, retro: "", wantRetro: false},
		{name: "сверх лимита — ретро дропается", post: strings.Repeat("а", digestPostMaxUTF16), retro: "🕰 ретро", wantRetro: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := appendRetro(c.post, c.retro)
			if c.wantRetro {
				want := c.post + "\n\n" + c.retro
				if got != want {
					t.Fatalf("appendRetro = %q, want %q", got, want)
				}
				return
			}
			if got != c.post {
				t.Fatalf("appendRetro = %q, want исходный пост без ретро", got)
			}
		})
	}
}

func TestUTF16Len(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"дайджест", 8}, // кириллица — BMP, 1 unit на символ
		{"🕰", 2},        // U+1F570 вне BMP — суррогатная пара
		{"a🕰b", 4},      // смешанный
	}
	for _, c := range cases {
		if got := utf16Len(c.in); got != c.want {
			t.Errorf("utf16Len(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestPastWeekCandidatesFeb29Start — старт 29 февраля: AddDate(-1) нормализует в 1 марта
// (окно съезжает на сутки — принято; ширина при этом не искажается).
func TestPastWeekCandidatesFeb29Start(t *testing.T) {
	start := time.Date(2024, 2, 29, 9, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	cands := pastWeekCandidates(start, end, 1)
	wantStart := time.Date(2023, 3, 1, 9, 0, 0, 0, time.UTC)
	if !cands[0].Start.Equal(wantStart) {
		t.Fatalf("старт окна = %v, want %v (нормализация 29 февраля)", cands[0].Start, wantStart)
	}
	if got := cands[0].End.Sub(cands[0].Start); got != 7*24*time.Hour {
		t.Fatalf("ширина = %v, want 168h", got)
	}
}

func TestIsSkipText(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"SKIP", true},
		{"  skip  ", true},
		{"SKIP\n", true},
		{"", false},
		{"Вот пункты", false},
	}
	for _, c := range cases {
		if got := isSkipText(c.in); got != c.want {
			t.Errorf("isSkipText(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
