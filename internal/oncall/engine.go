package oncall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"phpbot/internal/messages"
)

// Фазы сессии (oncall_sessions.phase).
const (
	phaseBrief      = "brief"
	phaseVote       = "vote"
	phaseOutcome    = "outcome"
	phasePostmortem = "postmortem"
)

const (
	statusActive   = "active"
	statusFinished = "finished"
)

// Тайминги рубрики и окно старта.
const (
	briefPhase   = 25 * time.Minute // сбор предложений
	votePhase    = 10 * time.Minute // голосование
	collectGrace = time.Minute      // грэйс после дедлайна: async-спам-классификация пишет в messages до ~40с позже
	maxRounds    = 3
	startHour    = 19
	startWindow  = 30 // минут: старт-окно 19:00–19:29

	recentTitlesLimit = 20
	maxProposals      = 40
	maxProposalRunes  = 400
)

// conservativePlan — авто-план бота при тишине раунда: играем консервативно, БЕЗ LLM.
var conservativePlan = Plan{
	Title:   "Наблюдение",
	Summary: "Ничего не менять: собрать метрики, slow log, наблюдать динамику",
}

// silentOutcomeText — статичная развязка тихого раунда (LLM не вызывается).
const silentOutcomeText = "Дежурных на смене нет — играем консервативно: наблюдение, сбор метрик и slow log, ничего не меняем. Динамика инцидента сохраняется: картина не улучшилась, но и не усугубилась."

const silentComplicationText = "Без активных действий часть признаков остаётся незамеченной — полной картины у смены нет."

// fallbackOutcome — развязка при сбое LLM: смена продолжается, состояние не ломаем.
func fallbackOutcome(state string) *Outcome {
	return &Outcome{
		Text:         "План применили, но картина противоречива: первичные сигналы не сошлись, статус инцидента пока не изменился.",
		Complication: "Мониторинг запаздывает: часть метрик ещё не отражает эффект действий, полной картины нет.",
		State:        state,
	}
}

// staticPostmortem — разбор при сбое LLM на финале.
func staticPostmortem() *Postmortem {
	return &Postmortem{
		Verdict: verdictPartial,
		Summary: "Автоматический разбор недоступен (LLM не ответил). Смена завершена по фактам раундов.",
	}
}

// Engine — state machine рубрики: один cron-тик, решения только по данным из БД
// (фаза + phase_deadline), без таймеров в памяти.
type Engine struct {
	svc        *Service
	repo       *Repository
	msgs       *messages.Repository
	onCallDays []time.Weekday
}

// NewEngine создаёт движок. onCallDays — дни рубрики (cfg.OnCallDays).
func NewEngine(svc *Service, repo *Repository, msgs *messages.Repository, onCallDays []time.Weekday) *Engine {
	return &Engine{svc: svc, repo: repo, msgs: msgs, onCallDays: onCallDays}
}

// Tick прогоняет все чаты: возможно, стартует смену и делает один шаг активной.
func (e *Engine) Tick(ctx context.Context) {
	now := time.Now()
	for _, chatID := range e.svc.chatIDs {
		if err := e.maybeStart(ctx, chatID, now); err != nil {
			slog.Warn("oncall maybe start", "err", err, "chat_id", chatID)
		}
		sess, err := e.repo.ActiveSession(ctx, chatID)
		if err != nil {
			slog.Warn("oncall active session", "err", err, "chat_id", chatID)
			continue
		}
		if sess == nil {
			continue
		}
		if err := e.stepSession(ctx, sess, now); err != nil {
			if errors.Is(err, ErrSessionFinished) {
				// /oncall cancel во время тика — норма, не шумим
				slog.Debug("oncall step: session finished concurrently", "session", sess.ID, "phase", sess.Phase)
				continue
			}
			slog.Warn("oncall step", "err", err, "session", sess.ID, "phase", sess.Phase, "round", sess.Round)
		}
	}
}

// maybeStart: день дежурства, время в окне 19:00–19:29, сегодня ещё не было смены
// и нет активной → старт. Ошибка старта не фатальна: следующий тик повторит.
func (e *Engine) maybeStart(ctx context.Context, chatID int64, now time.Time) error {
	if !isOnCallDay(e.onCallDays, now) || !inStartWindow(now) {
		return nil
	}
	sess, err := e.repo.ActiveSession(ctx, chatID)
	if err != nil {
		return err
	}
	if sess != nil {
		return nil
	}
	started, err := e.repo.StartedToday(ctx, chatID)
	if err != nil {
		return err
	}
	if started {
		return nil
	}
	return e.svc.StartSession(ctx, chatID)
}

// stepSession делает один шаг по фазе сессии. Фаза с phase_message_id=0 — resume:
// контент сохранён в БД, постим его и идём дальше.
func (e *Engine) stepSession(ctx context.Context, sess *SessionRow, now time.Time) error {
	switch sess.Phase {
	case phaseBrief:
		return e.stepBrief(ctx, sess, now)
	case phaseVote:
		return e.stepVote(ctx, sess, now)
	case phaseOutcome:
		return e.stepOutcome(ctx, sess, now)
	case phasePostmortem:
		return e.stepPostmortem(ctx, sess, now)
	default:
		slog.Warn("oncall unknown phase", "phase", sess.Phase, "session", sess.ID)
		return nil
	}
}

// stepBrief: ждём дедлайн+грэйс, читаем предложения чата за [phase_started, deadline).
// Тишина → авто-дежурный (консервативный план, без LLM) и рост счётчика тихих раундов.
// Предложения есть → кластеризация → persist фазы vote → сброс счётчика тишины → пост
// голосования. Сброс пишется только после успешного перехода фазы: сбой перехода не
// расходует «громкий» раунд, ретрай тика решает всё заново по данным из БД.
func (e *Engine) stepBrief(ctx context.Context, sess *SessionRow, now time.Time) error {
	if sess.PhaseMessageID == 0 {
		sc := parseScenario(sess.Scenario)
		sc.Title = sess.Title
		if err := e.svc.postBrief(ctx, sess.ID, sess.ChatID, sess.Round, sc, sess.PhaseDeadline); err != nil {
			return err
		}
	}
	if now.Before(sess.PhaseDeadline.Add(collectGrace)) {
		return nil
	}
	round, err := e.repo.RoundByNumber(ctx, sess.ID, sess.Round)
	if err != nil {
		return err
	}
	if round == nil {
		return fmt.Errorf("oncall round %d not found: session %d", sess.Round, sess.ID)
	}
	proposals, err := e.proposals(ctx, sess)
	if err != nil {
		return err
	}
	if len(proposals) == 0 {
		return e.silentRound(ctx, sess, round, now)
	}
	plans, err := e.svc.gen.ClusterPlans(ctx, proposals)
	if err != nil || len(plans) == 0 {
		// LLM не справился — чат был активен, срывать раунд нечестно: голосуем
		// консервативным планом.
		slog.Warn("oncall cluster plans fallback", "err", err)
		plans = []Plan{conservativePlan}
	}
	raw, err := json.Marshal(plans)
	if err != nil {
		return fmt.Errorf("oncall marshal plans: %w", err)
	}
	if err := e.repo.UpdateRoundPlans(ctx, round.ID, raw); err != nil {
		return err
	}
	deadline := now.Add(votePhase)
	if err := e.repo.SetPhase(ctx, sess.ID, phaseVote, now, deadline); err != nil {
		return err
	}
	if err := e.repo.ResetSilent(ctx, sess.ID); err != nil {
		slog.Warn("oncall reset silent", "err", err)
	}
	return e.svc.postVote(ctx, sess.ID, sess.ChatID, sess.Round, plans, nil, deadline)
}

// stepVote: ждём дедлайн → итог голосования → persist фазы outcome → развязка
// (LLM, fallback при сбое) → переход к следующему раунду или к разбору.
func (e *Engine) stepVote(ctx context.Context, sess *SessionRow, now time.Time) error {
	round, err := e.repo.RoundByNumber(ctx, sess.ID, sess.Round)
	if err != nil {
		return err
	}
	if round == nil {
		return fmt.Errorf("oncall round %d not found: session %d", sess.Round, sess.ID)
	}
	plans := round.ParsedPlans()
	if len(plans) == 0 {
		plans = []Plan{conservativePlan}
	}
	if sess.PhaseMessageID == 0 {
		if err := e.svc.postVote(ctx, sess.ID, sess.ChatID, sess.Round, plans, nil, sess.PhaseDeadline); err != nil {
			return err
		}
	}
	if now.Before(sess.PhaseDeadline) {
		return nil
	}
	counts, err := e.repo.BallotCounts(ctx, sess.ID, sess.Round)
	if err != nil {
		return err
	}
	idx := pickPlan(counts, len(plans))
	if err := e.repo.UpdateRoundResult(ctx, round.ID, idx, ""); err != nil {
		return err
	}
	if err := e.repo.SetPhase(ctx, sess.ID, phaseOutcome, now, now); err != nil {
		return err
	}
	if err := e.resolveAndPost(ctx, sess, round, plans, idx); err != nil {
		return err
	}
	return e.advance(ctx, sess)
}

// stepOutcome — resume-ветка фазы outcome: нормальный тик уводит фазу дальше в тот же
// проход, сюда попадаем только после сбоя между persist и постом.
func (e *Engine) stepOutcome(ctx context.Context, sess *SessionRow, now time.Time) error {
	round, err := e.repo.RoundByNumber(ctx, sess.ID, sess.Round)
	if err != nil {
		return err
	}
	if round == nil {
		return fmt.Errorf("oncall round %d not found: session %d", sess.Round, sess.ID)
	}
	plans := round.ParsedPlans()
	if len(plans) == 0 {
		plans = []Plan{conservativePlan}
	}
	idx := round.ChosenPlan
	if idx < 0 || idx >= len(plans) {
		counts, cerr := e.repo.BallotCounts(ctx, sess.ID, sess.Round)
		if cerr != nil {
			return cerr
		}
		idx = pickPlan(counts, len(plans))
		if err := e.repo.UpdateRoundResult(ctx, round.ID, idx, ""); err != nil {
			return err
		}
	}
	if round.Outcome == "" {
		if err := e.resolveAndPost(ctx, sess, round, plans, idx); err != nil {
			return err
		}
	} else if sess.PhaseMessageID == 0 {
		out := parseOutcome(round.Outcome)
		if err := e.svc.postPlain(ctx, sess.ID, sess.ChatID, renderOutcome(plans[idx], out, out.State)); err != nil {
			return err
		}
		if err := e.repo.SetState(ctx, sess.ID, out.State); err != nil {
			slog.Warn("oncall set state", "err", err)
		}
	}
	return e.advance(ctx, sess)
}

// stepPostmortem: разбор по раундам (LLM, fallback — статичный текст) → пост → Finish.
// phase_message_id != 0 — пост уже вышел (сбой до Finish), просто закрываем.
func (e *Engine) stepPostmortem(ctx context.Context, sess *SessionRow, now time.Time) error {
	if sess.PhaseMessageID != 0 {
		return e.repo.Finish(ctx, sess.ID, "")
	}
	rounds, err := e.repo.RoundsOfSession(ctx, sess.ID)
	if err != nil {
		return err
	}
	pm, err := e.svc.gen.Postmortem(ctx, roundBriefs(rounds), sess.State)
	if err != nil {
		slog.Warn("oncall postmortem fallback", "err", err)
		pm = staticPostmortem()
	}
	if err := e.svc.postPlain(ctx, sess.ID, sess.ChatID, renderFinal(pm, rounds)); err != nil {
		return err
	}
	return e.repo.Finish(ctx, sess.ID, pm.Verdict)
}

// silentRound: предложений нет — авто-дежурный играет консервативный план (без LLM).
// Вторая тишина подряд → мягкое завершение смены. Идемпотентность: решение о streak
// принимается по sess.SilentRounds+1 БЕЗ записи; запись счётчика — только после
// успешного перехода фазы (SetPhase/Finish). Сбой перехода → ретрай тика принимает то
// же решение по тем же данным, счётчик не задваивается и finishSilent не срабатывает
// преждевременно.
func (e *Engine) silentRound(ctx context.Context, sess *SessionRow, round *RoundRow, now time.Time) error {
	streak := sess.SilentRounds + 1
	slog.Info("oncall silent round", "session", sess.ID, "round", sess.Round, "streak", streak)
	if streak >= 2 {
		return e.finishSilent(ctx, sess, now)
	}
	rawPlans, _ := json.Marshal([]Plan{conservativePlan})
	if err := e.repo.UpdateRoundPlans(ctx, round.ID, rawPlans); err != nil {
		return err
	}
	out := &Outcome{Text: silentOutcomeText, Complication: silentComplicationText, State: sess.State}
	rawOut, _ := json.Marshal(out)
	if err := e.repo.UpdateRoundResult(ctx, round.ID, 0, string(rawOut)); err != nil {
		return err
	}
	if err := e.repo.SetPhase(ctx, sess.ID, phaseOutcome, now, now); err != nil {
		return err
	}
	if _, err := e.repo.BumpSilent(ctx, sess.ID); err != nil {
		slog.Warn("oncall bump silent", "err", err)
	}
	if err := e.svc.postPlain(ctx, sess.ID, sess.ChatID, renderOutcome(conservativePlan, out, sess.State)); err != nil {
		return err
	}
	if sess.Round >= maxRounds {
		// тишина на последнем раунде — мягкое завершение, полного разбора не будет
		return e.finishSilent(ctx, sess, now)
	}
	return e.advance(ctx, sess)
}

// resolveAndPost: LLM-развязка выбранного плана (сбой → статичный fallback, смена
// продолжается), persist текста развязки → пост → обновление состояния.
func (e *Engine) resolveAndPost(ctx context.Context, sess *SessionRow, round *RoundRow, plans []Plan, idx int) error {
	out, err := e.svc.gen.ResolveOutcome(ctx, plans[idx].Title, plans[idx].Summary, sess.State)
	if err != nil {
		slog.Warn("oncall resolve outcome fallback", "err", err)
		out = fallbackOutcome(sess.State)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("oncall marshal outcome: %w", err)
	}
	if err := e.repo.UpdateRoundResult(ctx, round.ID, idx, string(raw)); err != nil {
		return err
	}
	if err := e.svc.postPlain(ctx, sess.ID, sess.ChatID, renderOutcome(plans[idx], out, out.State)); err != nil {
		return err
	}
	if err := e.repo.SetState(ctx, sess.ID, out.State); err != nil {
		slog.Warn("oncall set state", "err", err)
	}
	return nil
}

// advance: после развязки — следующий раунд (сценарий → persist brief → пост) либо
// persist фазы postmortem (разбор сделает следующий тик — бюджет LLM на тик ≤2 вызова).
func (e *Engine) advance(ctx context.Context, sess *SessionRow) error {
	if sess.Round >= maxRounds {
		return e.repo.SetPhase(ctx, sess.ID, phasePostmortem, time.Now(), time.Now())
	}
	next := sess.Round + 1
	ban, err := e.repo.RecentTitles(ctx, sess.ChatID, recentTitlesLimit)
	if err != nil {
		slog.Warn("oncall recent titles", "err", err)
		ban = nil
	}
	sc, err := e.svc.gen.GenerateScenario(ctx, sess.ChatID, next, sess.State, ban)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("oncall marshal scenario: %w", err)
	}
	now := time.Now()
	deadline := now.Add(briefPhase)
	if _, err := e.repo.InsertRound(ctx, sess.ID, sess.ChatID, next, sc.Title, string(raw)); err != nil {
		return err
	}
	if err := e.repo.SetScenario(ctx, sess.ID, sc.Title, string(raw)); err != nil {
		return err
	}
	if err := e.repo.SetRound(ctx, sess.ID, next); err != nil {
		return err
	}
	if err := e.repo.SetPhase(ctx, sess.ID, phaseBrief, now, deadline); err != nil {
		return err
	}
	return e.svc.postBrief(ctx, sess.ID, sess.ChatID, next, sc, deadline)
}

// finishSilent — мягкое завершение при двух тихих раундах подряд: короткий статичный
// текст + LLM-разбор, если LLM доступен.
func (e *Engine) finishSilent(ctx context.Context, sess *SessionRow, now time.Time) error {
	if err := e.repo.SetPhase(ctx, sess.ID, phasePostmortem, now, now); err != nil {
		return err
	}
	rounds, err := e.repo.RoundsOfSession(ctx, sess.ID)
	if err != nil {
		return err
	}
	verdict := verdictFailed
	pm, perr := e.svc.gen.Postmortem(ctx, roundBriefs(rounds), sess.State)
	if perr != nil {
		slog.Warn("oncall silent postmortem fallback", "err", perr)
		pm = nil
	} else {
		verdict = pm.Verdict
	}
	if err := e.svc.postPlain(ctx, sess.ID, sess.ChatID, renderSilentFinish(pm)); err != nil {
		return err
	}
	return e.repo.Finish(ctx, sess.ID, verdict)
}

// proposals читает тексты сообщений чата за фазу сбора [phase_started, deadline).
// Свои посты бот в messages не пишет (не получает их update'ом), команды туда тоже
// не попадают — это чистые предложения дежурных.
func (e *Engine) proposals(ctx context.Context, sess *SessionRow) ([]string, error) {
	rows, err := e.msgs.Since(ctx, sess.ChatID, sess.PhaseStarted, sess.PhaseDeadline)
	if err != nil {
		return nil, fmt.Errorf("oncall proposals: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, m := range rows {
		t := strings.TrimSpace(m.Text)
		if t == "" {
			continue
		}
		out = append(out, truncateRunes(t, maxProposalRunes))
		if len(out) >= maxProposals {
			break
		}
	}
	return out, nil
}

// roundBriefs собирает факты раундов для посмертного разбора.
func roundBriefs(rounds []RoundRow) []RoundBrief {
	out := make([]RoundBrief, 0, len(rounds))
	for _, r := range rounds {
		b := RoundBrief{Round: r.Round, Title: r.Title, Plan: "—"}
		if p := r.ParsedPlans(); r.ChosenPlan >= 0 && r.ChosenPlan < len(p) {
			b.Plan = p[r.ChosenPlan].Title
		}
		if r.Outcome != "" {
			b.Outcome = truncateRunes(parseOutcome(r.Outcome).Text, 300)
		}
		out = append(out, b)
	}
	return out
}

// pickPlan — чистая: план с максимумом голосов; при нуле голосов или ничьей —
// индекс 0 (планы идут от консервативного к рискованному).
func pickPlan(counts map[int]int, nPlans int) int {
	best, bestVotes := 0, 0
	for i := 0; i < nPlans; i++ {
		if counts[i] > bestVotes {
			best, bestVotes = i, counts[i]
		}
	}
	return best
}

// isOnCallDay — сегодня день рубрики.
func isOnCallDay(days []time.Weekday, now time.Time) bool {
	for _, d := range days {
		if d == now.Weekday() {
			return true
		}
	}
	return false
}

// inStartWindow — время в старт-окне 19:00–19:29 локальное.
func inStartWindow(now time.Time) bool {
	return now.Hour() == startHour && now.Minute() < startWindow
}
