package oncall

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"phpbot/internal/md"
)

func TestCapPost(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short_unchanged", "короткий текст", "короткий текст"},
		{"ascii_at_limit_unchanged", strings.Repeat("a", postMaxUTF16), strings.Repeat("a", postMaxUTF16)},
		{"cyrillic_at_limit_unchanged", strings.Repeat("а", postMaxUTF16), strings.Repeat("а", postMaxUTF16)},
		{"emoji_exact_limit_unchanged", strings.Repeat("👁", postMaxUTF16/2), strings.Repeat("👁", postMaxUTF16/2)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := capPost(c.in); got != c.want {
				t.Errorf("%s: capPost изменил строку в пределах лимита (got len %d units)", c.name, md.UTF16Len(got))
			}
		})
	}
	t.Run("ascii_over_by_one_cut_to_limit", func(t *testing.T) {
		got := capPost(strings.Repeat("a", postMaxUTF16+1))
		want := strings.Repeat("a", postMaxUTF16-1) + "…"
		if got != want {
			t.Errorf("got %d units, want ровно %d символов + «…»", md.UTF16Len(got), postMaxUTF16-1)
		}
	})
	t.Run("emoji_overflow_counted_as_two", func(t *testing.T) {
		got := capPost(strings.Repeat("👁", postMaxUTF16)) // 7000 UTF-16 units
		if n := md.UTF16Len(got); n > postMaxUTF16 {
			t.Errorf("эмодзи-переполнение: len %d > %d", n, postMaxUTF16)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("обрезанный пост должен кончаться «…»")
		}
	})
	t.Run("long_post_keeps_header", func(t *testing.T) {
		header := "🚨 ДЕЖУРСТВО — РАУНД 1/3\n"
		got := capPost(header + strings.Repeat("текст инцидента ", 500))
		if !strings.HasPrefix(got, header) {
			t.Errorf("шапка поста потеряна при обрезке: %q…", got[:min(40, len(got))])
		}
		if n := md.UTF16Len(got); n > postMaxUTF16 {
			t.Errorf("len %d > %d", n, postMaxUTF16)
		}
	})
}

func TestRenderBrief(t *testing.T) {
	deadline := time.Date(2026, 10, 7, 19, 24, 0, 0, time.Local)
	cases := []struct {
		name    string
		round   int
		sc      *Scenario
		want    []string
		wantNot []string
	}{
		{"full_scenario", 1, &Scenario{Title: "Отвал основной БД", Severity: sev1, Narrative: "Реплика ушла в retry-шторм", Metrics: "err 500: 12%"},
			[]string{"РАУНД 1/3", "SEV1", "Отвал основной БД", "Реплика ушла в retry-шторм", "Метрики", "err 500: 12%", "до 19:24"}, nil},
		{"round_two", 2, &Scenario{Title: "T", Severity: sev2, Narrative: "N"},
			[]string{"РАУНД 2/3", "SEV2"}, nil},
		{"no_metrics", 3, &Scenario{Title: "T", Severity: sev3, Narrative: "N"},
			[]string{"SEV3"}, []string{"Метрики"}},
		{"empty_scenario_fail_safe", 1, &Scenario{},
			[]string{"РАУНД 1/3", "SEV?"}, []string{"Метрики"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderBrief(c.round, maxRounds, c.sc, deadline)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("%s: нет %q в:\n%s", c.name, w, got)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("%s: лишний %q в:\n%s", c.name, w, got)
				}
			}
		})
	}
}

func TestRenderVote(t *testing.T) {
	deadline := time.Date(2026, 10, 7, 20, 4, 0, 0, time.Local)
	plans := []Plan{
		{Title: "Наблюдение", Summary: "собрать метрики"},
		{Title: "Откат", Summary: "вернуть прошлую версию"},
	}
	cases := []struct {
		name    string
		plans   []Plan
		counts  map[int]int
		want    []string
		wantNot []string
	}{
		{"plans_with_tally", plans, map[int]int{0: 2, 1: 1},
			[]string{"РАУНД 2/3", "ГОЛОСОВАНИЕ", "1. **Наблюдение**", "собрать метрики", "2. **Откат**", "вернуть прошлую версию", "Голосов: 3", "консервативный", "до 20:04"}, nil},
		{"zero_votes_no_tally", plans, nil,
			[]string{"1. **Наблюдение**"}, []string{"Голосов"}},
		{"empty_counts_no_tally", plans, map[int]int{},
			[]string{"ГОЛОСОВАНИЕ"}, []string{"Голосов"}},
		{"no_plans_nil_safe", nil, nil,
			[]string{"ГОЛОСОВАНИЕ", "консервативный"}, []string{"1."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderVote(2, maxRounds, c.plans, c.counts, deadline)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("%s: нет %q в:\n%s", c.name, w, got)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("%s: лишний %q в:\n%s", c.name, w, got)
				}
			}
		})
	}
}

func TestRenderOutcome(t *testing.T) {
	chosen := Plan{Title: "Откат деплоя", Summary: "вернуть прошлую версию"}
	out := &Outcome{Text: "Ошибка ушла", Complication: "Часть очередей отстала", State: "err 0.1%"}
	cases := []struct {
		name    string
		chosen  Plan
		out     *Outcome
		state   string
		want    []string
		wantNot []string
	}{
		{"full_state_from_outcome", chosen, out, "",
			[]string{"РАЗВЯЗКА", "**Откат деплоя**", "вернуть прошлую версию", "Что произошло", "Ошибка ушла", "Осложнение", "Часть очередей отстала", "Метрики", "err 0.1%"}, nil},
		{"state_param_overrides_out_state", chosen, out, "свежие метрики",
			[]string{"свежие метрики"}, []string{"err 0.1%"}},
		{"no_complication", chosen, &Outcome{Text: "Т"}, "",
			[]string{"Что произошло"}, []string{"Осложнение"}},
		{"both_states_empty_no_metrics", chosen, &Outcome{Text: "Т"}, "",
			[]string{"Что произошло"}, []string{"Метрики"}},
		{"empty_fields_fail_safe", Plan{}, &Outcome{}, "",
			[]string{"РАЗВЯЗКА"}, []string{"Метрики", "Осложнение"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderOutcome(c.chosen, c.out, c.state)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("%s: нет %q в:\n%s", c.name, w, got)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("%s: лишний %q в:\n%s", c.name, w, got)
				}
			}
		})
	}
}

func TestRenderFinal(t *testing.T) {
	plansJSON := []byte(`[{"title":"Откат","summary":"s"}]`)
	rounds := []RoundRow{
		{Round: 1, Title: "Отвал БД", Plans: plansJSON, ChosenPlan: 0},
		{Round: 2, Title: "Эскалация", ChosenPlan: -1},
	}
	cases := []struct {
		name    string
		pm      *Postmortem
		rounds  []RoundRow
		want    []string
		wantNot []string
	}{
		{"full", &Postmortem{Verdict: verdictClosed, Summary: "Инцидент закрыт за смену", Right: "быстрый откат", Wrong: "поздний алерт"}, rounds,
			[]string{"ИСХОД", "инцидент закрыт (closed)", "ПОСМЕРТНЫЙ РАЗБОР", "Инцидент закрыт за смену", "Верно", "быстрый откат", "Ошибки", "поздний алерт", "Хронология", "Раунд 1. Отвал БД → «Откат»", "план не выбран"}, nil},
		{"minimal_no_optional_blocks", &Postmortem{Verdict: verdictFailed, Summary: "S"}, nil,
			[]string{"провал смены (failed)", "S"}, []string{"Верно", "Ошибки", "Хронология"}},
		{"partial_verdict", &Postmortem{Verdict: verdictPartial, Summary: "S"}, nil,
			[]string{"закрыт частично (partial)"}, nil},
		{"unknown_verdict_labelled", &Postmortem{Verdict: "bogus", Summary: "S"}, nil,
			[]string{"вердикт неизвестен"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderFinal(c.pm, c.rounds)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("%s: нет %q в:\n%s", c.name, w, got)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("%s: лишний %q в:\n%s", c.name, w, got)
				}
			}
		})
	}
}

func TestRenderSilentFinish(t *testing.T) {
	cases := []struct {
		name    string
		pm      *Postmortem
		want    []string
		wantNot []string
	}{
		{"nil_pm_static_only", nil,
			[]string{"завершена досрочно", "передан следующей смене"}, []string{"ПОСМЕРТНЫЙ"}},
		{"with_pm_compact_brief", &Postmortem{Verdict: verdictPartial, Summary: "разбор по фактам"},
			[]string{"ПОСМЕРТНЫЙ РАЗБОР (закрыт частично)", "разбор по фактам"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderSilentFinish(c.pm)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("%s: нет %q в:\n%s", c.name, w, got)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("%s: лишний %q в:\n%s", c.name, w, got)
				}
			}
		})
	}
}

func TestRenderCancelled(t *testing.T) {
	if got := renderCancelled(); !strings.Contains(got, "Смена отменена") {
		t.Errorf("renderCancelled = %q, want текст об отмене", got)
	}
}

func TestSeverityLabel(t *testing.T) {
	cases := []struct {
		name string
		sev  string
		want string
	}{
		{"sev1", sev1, "SEV1 · критичный"},
		{"sev2", sev2, "SEV2 · деградация"},
		{"sev3", sev3, "SEV3 · локальный"},
		{"unknown_empty", "", "SEV? · неизвестный"},
		{"unknown_sev4", "sev4", "SEV? · неизвестный"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := severityLabel(c.sev)
			if !strings.Contains(got, c.want) {
				t.Errorf("%s: severityLabel(%q) = %q, want содержит %q", c.name, c.sev, got, c.want)
			}
		})
	}
}

func TestVerdictLabel(t *testing.T) {
	cases := []struct {
		name string
		v    string
		want string
	}{
		{"closed", verdictClosed, "инцидент закрыт"},
		{"partial", verdictPartial, "закрыт частично"},
		{"failed", verdictFailed, "провал смены"},
		{"unknown_empty", "", "вердикт неизвестен"},
		{"unknown_word", "success", "вердикт неизвестен"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verdictLabel(c.v); got != c.want {
				t.Errorf("%s: verdictLabel(%q) = %q, want %q", c.name, c.v, got, c.want)
			}
		})
	}
}

func TestRoundLines(t *testing.T) {
	plansJSON := []byte(`[{"title":"Откат","summary":"s"}]`)
	cases := []struct {
		name   string
		rounds []RoundRow
		want   string
	}{
		{"chosen_plan_quoted", []RoundRow{{Round: 1, Title: "Завязка", Plans: plansJSON, ChosenPlan: 0}}, "Раунд 1. Завязка → «Откат»\n"},
		{"plan_not_chosen", []RoundRow{{Round: 2, Title: "T", ChosenPlan: -1}}, "Раунд 2. T → «план не выбран»\n"},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := roundLines(c.rounds); got != c.want {
				t.Errorf("%s: roundLines() = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

func TestVoteKeyboard(t *testing.T) {
	plans := []Plan{
		{Title: "Наблюдение", Summary: "s1"},
		{Title: "Откат", Summary: "s2"},
		{Title: "Перезагрузка", Summary: "s3"},
	}
	kb := voteKeyboard(42, 2, plans, map[int]int{1: 3})
	if len(kb.InlineKeyboard) != len(plans) {
		t.Fatalf("рядов %d, want %d (по одному плану в ряд)", len(kb.InlineKeyboard), len(plans))
	}
	wantText := []string{"1. Наблюдение", "2. Откат (3)", "3. Перезагрузка"}
	for i, row := range kb.InlineKeyboard {
		if len(row) != 1 {
			t.Errorf("case row %d: кнопок в ряду %d, want 1", i, len(row))
			continue
		}
		btn := row[0]
		if btn.Text != wantText[i] {
			t.Errorf("case row %d: Text = %q, want %q", i, btn.Text, wantText[i])
		}
		wantData := "oncall:42:2:" + strconv.Itoa(i)
		if btn.CallbackData != wantData {
			t.Errorf("case row %d: CallbackData = %q, want %q", i, btn.CallbackData, wantData)
		}
		// контракт HandleCallback: "oncall:<sid>:<round>:<planIdx>" — 4 поля
		parts := strings.Split(btn.CallbackData, ":")
		if len(parts) != 4 || parts[0] != "oncall" || parts[1] != "42" || parts[2] != "2" || parts[3] != strconv.Itoa(i) {
			t.Errorf("case row %d: %q не разбирается как oncall:<sid>:<round>:<planIdx>", i, btn.CallbackData)
		}
	}
	t.Run("zero_counts_no_counter", func(t *testing.T) {
		kb := voteKeyboard(42, 2, plans, nil)
		for i, row := range kb.InlineKeyboard {
			if strings.Contains(row[0].Text, "(") {
				t.Errorf("case row %d: лейбл %q не должен содержать счётчик при нуле голосов", i, row[0].Text)
			}
		}
	})
	t.Run("no_plans_empty_keyboard", func(t *testing.T) {
		if kb := voteKeyboard(1, 1, nil, nil); len(kb.InlineKeyboard) != 0 {
			t.Errorf("клавиатура без планов = %d рядов, want 0", len(kb.InlineKeyboard))
		}
	})
}

// Инвариант постинга: capPost ужимает markdown до бюджета рубрики, а после ToHTML
// пост обязан влезать в жёсткий лимит Telegram 4096 UTF-16 units (HTML-теги и
// экрейпинг добавляют длину к уже ужатому тексту).
func TestRenderFitsTelegramLimitAfterHTML(t *testing.T) {
	long := strings.Repeat("метрики деградируют, алерты сыплются, дежурный потеет ", 120)
	title := strings.Repeat("П", 60)
	deadline := time.Date(2026, 10, 7, 19, 24, 0, 0, time.Local)
	rounds := []RoundRow{
		{Round: 1, Title: title, Plans: []byte(`[{"title":"x","summary":"y"}]`), ChosenPlan: 0, Outcome: `{"outcome":"ok"}`},
		{Round: 2, Title: title, ChosenPlan: -1},
		{Round: 3, Title: title, ChosenPlan: -1},
	}
	renders := []struct {
		name   string
		render string
	}{
		{"brief", renderBrief(2, maxRounds, &Scenario{Title: title, Severity: sev1, Narrative: long, Metrics: long}, deadline)},
		{"vote", renderVote(2, maxRounds, []Plan{{Title: title, Summary: long}, {Title: title, Summary: long}, {Title: title, Summary: long}, {Title: title, Summary: long}}, map[int]int{0: 3, 2: 1}, deadline)},
		{"outcome", renderOutcome(Plan{Title: title, Summary: long}, &Outcome{Text: long, Complication: long, State: long}, long)},
		{"final", renderFinal(&Postmortem{Verdict: verdictPartial, Summary: long, Right: long, Wrong: long}, rounds)},
		{"silent_finish", renderSilentFinish(&Postmortem{Verdict: verdictFailed, Summary: long})},
	}
	for _, r := range renders {
		t.Run(r.name, func(t *testing.T) {
			capped := capPost(r.render)
			if n := md.UTF16Len(capped); n > postMaxUTF16 {
				t.Errorf("capPost: len %d > бюджета %d", n, postMaxUTF16)
			}
			final := md.ToHTML(capped)
			if n := md.UTF16Len(final); n > 4096 {
				t.Errorf("ToHTML(capPost(...)): len %d > лимита Telegram 4096", n)
			}
		})
	}
}

// TestRenderDenseSpecialsSurviveHTML — плотность ' & < > " в LLM-поле: рендер +
// capPost + md.ToHTML обязан давать валидный Telegram-HTML — спецсимволы экранированы
// (&#39;/&#34;/&amp;/&lt;/&gt;), бюджет 4096 не превышен. Патологическая плотность во
// ВСЕХ полях разом раздувает HTML за 4096 (экрейпинг расширяет текст) — такой пост
// Telegram отвергает, и его выручает plain-fallback в sendHTML (второй шанс тем же
// текстом без ParseMode).
func TestRenderDenseSpecialsSurviveHTML(t *testing.T) {
	dense := strings.Repeat("err 'timeout' & <upstream> \"quoted\" ", 40)
	deadline := time.Date(2026, 10, 7, 19, 24, 0, 0, time.Local)
	renders := []struct {
		name   string
		render string
	}{
		{"brief", renderBrief(1, maxRounds, &Scenario{Title: "T", Severity: sev1, Narrative: dense}, deadline)},
		{"vote", renderVote(1, maxRounds, []Plan{{Title: "T", Summary: dense}}, nil, deadline)},
		{"outcome", renderOutcome(Plan{Title: "T", Summary: "s"}, &Outcome{Text: dense}, "")},
		{"final", renderFinal(&Postmortem{Verdict: verdictClosed, Summary: dense}, nil)},
		{"silent_finish", renderSilentFinish(&Postmortem{Verdict: verdictFailed, Summary: dense})},
	}
	for _, r := range renders {
		t.Run(r.name, func(t *testing.T) {
			capped := capPost(r.render)
			if n := md.UTF16Len(capped); n > postMaxUTF16 {
				t.Errorf("capPost: len %d > бюджета %d", n, postMaxUTF16)
			}
			final := md.ToHTML(capped)
			if n := md.UTF16Len(final); n > 4096 {
				t.Errorf("HTML: len %d UTF-16 units > лимита Telegram 4096", n)
			}
			for _, esc := range []string{"&lt;", "&gt;", "&amp;", "&#39;", "&#34;"} {
				if !strings.Contains(final, esc) {
					t.Errorf("спецсимволы не экранированы: нет %q в HTML", esc)
				}
			}
		})
	}
}

// TestShouldFinishAfterPostFail — порог счётчика неудачных постов: до postFailLimit
// сессия живёт (тик повторит отправку), с postFailLimit — закрывается (перманентный
// отказ постинга, напр. бот кикнут).
func TestShouldFinishAfterPostFail(t *testing.T) {
	cases := []struct {
		name     string
		attempts int
		want     bool
	}{
		{"zero_continue", 0, false},
		{"just_below_limit_continue", postFailLimit - 1, false},
		{"at_limit_finish", postFailLimit, true},
		{"above_limit_finish", postFailLimit + 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldFinishAfterPostFail(c.attempts); got != c.want {
				t.Errorf("%s: shouldFinishAfterPostFail(%d) = %v, want %v", c.name, c.attempts, got, c.want)
			}
		})
	}
}

// TestVoteWindowOpen — поздние голоса: клик по кнопке после дедлайна фазы (или в
// другой фазе) окно закрыто, голос не записывается. Граница = stepVote: при
// now >= deadline голосование закрыто.
func TestVoteWindowOpen(t *testing.T) {
	deadline := time.Date(2026, 10, 7, 20, 4, 0, 0, time.Local)
	cases := []struct {
		name string
		sess *SessionRow
		now  time.Time
		want bool
	}{
		{"vote_phase_before_deadline_open", &SessionRow{Phase: phaseVote, PhaseDeadline: deadline}, deadline.Add(-time.Minute), true},
		{"exactly_deadline_closed", &SessionRow{Phase: phaseVote, PhaseDeadline: deadline}, deadline, false},
		{"after_deadline_closed", &SessionRow{Phase: phaseVote, PhaseDeadline: deadline}, deadline.Add(time.Second), false},
		{"brief_phase_closed", &SessionRow{Phase: phaseBrief, PhaseDeadline: deadline}, deadline.Add(-time.Hour), false},
		{"outcome_phase_closed", &SessionRow{Phase: phaseOutcome, PhaseDeadline: deadline}, deadline.Add(-time.Hour), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := voteWindowOpen(c.sess, c.now); got != c.want {
				t.Errorf("%s: voteWindowOpen(phase=%s, deadline=%s, now=%s) = %v, want %v",
					c.name, c.sess.Phase, c.sess.PhaseDeadline.Format("15:04:05"), c.now.Format("15:04:05"), got, c.want)
			}
		})
	}
}
