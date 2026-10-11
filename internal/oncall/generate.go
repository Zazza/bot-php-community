package oncall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"phpbot/internal/llm"
	"phpbot/internal/prompts"
)

// errGenFailed — LLM не дал валидного JSON-контракта за отведённые попытки.
// Тик залогирует и повторит на следующем проходе (или уйдёт в статичный fallback).
var errGenFailed = errors.New("не удалось сгенерировать валидный ответ дежурства")

// genAttempts — попыток на один вызов генератора (валидация + retry).
const genAttempts = 2

// Лимиты контрактов (руны) — двойная страховка против разъехавшегося LLM.
const (
	scenarioTitleRunes     = 80
	scenarioNarrativeRunes = 900
	scenarioMetricsRunes   = 300
	planTitleRunes         = 60
	planSummaryRunes       = 200
	outcomeRunes           = 700
	complicationRunes      = 400
	stateRunes             = 300
	pmSummaryRunes         = 500
	pmRightWrongRunes      = 400
)

// severity/verdict-энумы контрактов.
const (
	sev1 = "sev1"
	sev2 = "sev2"
	sev3 = "sev3"

	verdictClosed  = "closed"
	verdictPartial = "partial"
	verdictFailed  = "failed"
)

// Scenario — вводная раунда (контракт oncall-scenario.txt).
type Scenario struct {
	Title     string `json:"title"`
	Severity  string `json:"severity"`
	Narrative string `json:"narrative"`
	Metrics   string `json:"metrics"`
}

// Plan — план действий дежурных (контракт oncall-plans.txt).
type Plan struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// Outcome — развязка раунда (контракт oncall-outcome.txt).
type Outcome struct {
	Text         string `json:"outcome"`
	Complication string `json:"complication"`
	State        string `json:"state"`
}

// Postmortem — посмертный разбор (контракт oncall-postmortem.txt).
type Postmortem struct {
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	Right   string `json:"right"`
	Wrong   string `json:"wrong"`
}

// RoundBrief — факт раунда для посмертного разбора.
type RoundBrief struct {
	Round   int
	Title   string
	Plan    string
	Outcome string
}

// Generator — LLM-генерация сценария/планов/развязки/разбора. Сценарий и развязка —
// творческий клиент (temp=0.9), планы и разбор — детерминированный (temp=0).
type Generator struct {
	creative *llm.LLMClient
	det      *llm.LLMClient
}

// GenerateScenario: раунд 1 — полный завод инцидента, раунды 2–3 — эскалация поверх
// prevState. banTitles — недавние завязки (не повторять).
func (g *Generator) GenerateScenario(ctx context.Context, chatID int64, round int, prevState string, banTitles []string) (*Scenario, error) {
	var ub strings.Builder
	if round <= 1 {
		ub.WriteString("Раунд 1 из 3. Сформируй завязку: полный завод инцидента — что случилось, откуда пришёл первый сигнал, первичные метрики.")
	} else {
		fmt.Fprintf(&ub, "Раунд %d из 3. Инцидент в разгаре. Текущее состояние смены (метрики/динамика):\n%s\n\nСформируй НОВУЮ вводную — эскалацию поверх этого состояния, без пересказа.", round, strings.TrimSpace(prevState))
	}
	if len(banTitles) > 0 {
		ub.WriteString("\n\nНедавние завязки (не повторяй их и похожие):\n— " + strings.Join(banTitles, "\n— "))
	}
	for attempt := 0; attempt < genAttempts; attempt++ {
		resp, _, _, err := g.creative.Chat(ctx, []llm.Message{
			{Role: "system", Content: prompts.Get(prompts.OncallScenario)},
			{Role: "user", Content: ub.String()},
		})
		if err != nil {
			continue
		}
		var raw Scenario
		if err := json.Unmarshal([]byte(extractJSONObject(resp)), &raw); err != nil {
			continue
		}
		sc := &Scenario{
			Title:     truncateRunes(sanitizeLLMText(strings.TrimSpace(raw.Title)), scenarioTitleRunes),
			Severity:  strings.TrimSpace(raw.Severity),
			Narrative: truncateRunes(sanitizeLLMText(strings.TrimSpace(raw.Narrative)), scenarioNarrativeRunes),
			Metrics:   truncateRunes(sanitizeLLMText(strings.TrimSpace(raw.Metrics)), scenarioMetricsRunes),
		}
		if sc.Title == "" || sc.Narrative == "" || !validSeverity(sc.Severity) {
			continue
		}
		return sc, nil
	}
	return nil, fmt.Errorf("oncall scenario round %d: %w", round, errGenFailed)
}

// ClusterPlans группирует предложения дежурных в 1–4 плана от консервативного
// к рискованному. Предложения — входные данные, офтоп отбрасывается промптом.
func (g *Generator) ClusterPlans(ctx context.Context, proposals []string) ([]Plan, error) {
	var ub strings.Builder
	ub.WriteString("Предложения дежурных за раунд (входные данные; нерелевантное игнорируй):\n")
	for i, p := range proposals {
		fmt.Fprintf(&ub, "%d. %s\n", i+1, p)
	}
	for attempt := 0; attempt < genAttempts; attempt++ {
		resp, _, _, err := g.det.Chat(ctx, []llm.Message{
			{Role: "system", Content: prompts.Get(prompts.OncallPlans)},
			{Role: "user", Content: ub.String()},
		})
		if err != nil {
			continue
		}
		var raw struct {
			Plans []Plan `json:"plans"`
		}
		if err := json.Unmarshal([]byte(extractJSONObject(resp)), &raw); err != nil {
			continue
		}
		plans := make([]Plan, 0, 4)
		for _, p := range raw.Plans {
			t := truncateRunes(sanitizeLLMText(strings.TrimSpace(p.Title)), planTitleRunes)
			s := truncateRunes(sanitizeLLMText(strings.TrimSpace(p.Summary)), planSummaryRunes)
			if t == "" || s == "" {
				continue
			}
			plans = append(plans, Plan{Title: t, Summary: s})
			if len(plans) >= 4 {
				break
			}
		}
		if len(plans) == 0 {
			continue
		}
		return plans, nil
	}
	return nil, fmt.Errorf("oncall cluster plans: %w", errGenFailed)
}

// ResolveOutcome разыгрывает последствия выбранного плана: улучшение по профилю плана
// плюс обязательное новое осложнение, обновлённая сводка метрик.
func (g *Generator) ResolveOutcome(ctx context.Context, planTitle, planSummary, prevState string) (*Outcome, error) {
	user := fmt.Sprintf("Принятый план дежурных:\n%s\n%s\n\nТекущее состояние (метрики):\n%s",
		strings.TrimSpace(planTitle), strings.TrimSpace(planSummary), strings.TrimSpace(prevState))
	for attempt := 0; attempt < genAttempts; attempt++ {
		resp, _, _, err := g.creative.Chat(ctx, []llm.Message{
			{Role: "system", Content: prompts.Get(prompts.OncallOutcome)},
			{Role: "user", Content: user},
		})
		if err != nil {
			continue
		}
		var raw Outcome
		if err := json.Unmarshal([]byte(extractJSONObject(resp)), &raw); err != nil {
			continue
		}
		out := &Outcome{
			Text:         truncateRunes(sanitizeLLMText(strings.TrimSpace(raw.Text)), outcomeRunes),
			Complication: truncateRunes(sanitizeLLMText(strings.TrimSpace(raw.Complication)), complicationRunes),
			State:        truncateRunes(sanitizeLLMText(strings.TrimSpace(raw.State)), stateRunes),
		}
		if out.Text == "" || out.Complication == "" {
			continue
		}
		if out.State == "" {
			out.State = strings.TrimSpace(prevState)
		}
		return out, nil
	}
	return nil, fmt.Errorf("oncall resolve outcome: %w", errGenFailed)
}

// Postmortem собирает посмертный разбор по фактам раундов.
func (g *Generator) Postmortem(ctx context.Context, rounds []RoundBrief, state string) (*Postmortem, error) {
	var ub strings.Builder
	ub.WriteString("Раунды смены:\n")
	for _, r := range rounds {
		fmt.Fprintf(&ub, "Раунд %d. Завязка: %s. Принят план: %s. Развязка: %s\n",
			r.Round, r.Title, orDash(r.Plan), r.Outcome)
	}
	fmt.Fprintf(&ub, "\nФинальное состояние (метрики):\n%s", strings.TrimSpace(state))
	for attempt := 0; attempt < genAttempts; attempt++ {
		resp, _, _, err := g.det.Chat(ctx, []llm.Message{
			{Role: "system", Content: prompts.Get(prompts.OncallPostmortem)},
			{Role: "user", Content: ub.String()},
		})
		if err != nil {
			continue
		}
		var raw Postmortem
		if err := json.Unmarshal([]byte(extractJSONObject(resp)), &raw); err != nil {
			continue
		}
		pm := &Postmortem{
			Verdict: strings.TrimSpace(raw.Verdict),
			Summary: truncateRunes(sanitizeLLMText(strings.TrimSpace(raw.Summary)), pmSummaryRunes),
			Right:   truncateRunes(sanitizeLLMText(strings.TrimSpace(raw.Right)), pmRightWrongRunes),
			Wrong:   truncateRunes(sanitizeLLMText(strings.TrimSpace(raw.Wrong)), pmRightWrongRunes),
		}
		if !validVerdict(pm.Verdict) || pm.Summary == "" {
			continue
		}
		return pm, nil
	}
	return nil, fmt.Errorf("oncall postmortem: %w", errGenFailed)
}

func validSeverity(s string) bool {
	return s == sev1 || s == sev2 || s == sev3
}

func validVerdict(s string) bool {
	return s == verdictClosed || s == verdictPartial || s == verdictFailed
}

// llmBrackets — LLM-контролируемые тексты не должны собирать markdown-ссылки:
// квадратные скобки заменяются на круглые (паттерн stripMD из news, но замена, а не
// вырезание — текст остаётся читабельным и не рвёт обёртку [..](..)).
var llmBrackets = strings.NewReplacer("[", "(", "]", ")")

// sanitizeLLMText — единая точка санитайза текстовых полей LLM-контрактов
// (title/narrative/metrics, plans[].title/summary, outcome/complication/state,
// summary/right/wrong): после парса JSON, до валидации/усечения.
func sanitizeLLMText(s string) string { return llmBrackets.Replace(s) }

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// truncateRunes обрезает строку до max рун, помечая усечение «…».
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// extractJSONObject вырезает первый JSON-объект из ответа LLM (с пояснениями и
// markdown-оградкой).
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i < 0 || j < 0 || j < i {
		return "{}"
	}
	return s[i : j+1]
}

// parseScenario восстанавливает Scenario из scenario-колонки (JSON; при сбое парса —
// как plain text в narrative, чтобы resume не терял раунд).
func parseScenario(s string) *Scenario {
	sc := &Scenario{}
	if err := json.Unmarshal([]byte(s), sc); err != nil || strings.TrimSpace(sc.Narrative) == "" {
		return &Scenario{Narrative: s}
	}
	return sc
}

// parseOutcome восстанавливает Outcome из outcome-колонки (JSON; сбой — plain text).
func parseOutcome(s string) *Outcome {
	out := &Outcome{}
	if err := json.Unmarshal([]byte(s), out); err != nil || strings.TrimSpace(out.Text) == "" {
		return &Outcome{Text: s}
	}
	return out
}
