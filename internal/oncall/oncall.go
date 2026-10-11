package oncall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"phpbot/internal/llm"
	"phpbot/internal/md"
	"phpbot/internal/messages"
)

// ErrActiveSession — в чате уже идёт смена дежурства.
var ErrActiveSession = errors.New("уже есть активная смена дежурства")

// ErrNoSession — активной смены нет.
var ErrNoSession = errors.New("нет активной смены дежурства")

// postMaxUTF16 — бюджет одного поста рубрики в UTF-16 code units (TG-лимит 4096,
// рубрика укладывается заметно меньше — режем с запасом).
const postMaxUTF16 = 3500

// postFailLimit — неудачных постов фазы подряд (HTML и plain оба отвергнуты), после
// которых сессия закрывается: перманентный отказ постинга (напр. бот кикнут из чата)
// иначе гонял бы resume-ретрай каждого тика вечно.
const postFailLimit = 5

// Service — старт/отмена сессии, голосование за планы, рендер и постинг.
type Service struct {
	api     *bot.Bot
	gen     *Generator
	repo    *Repository
	msgs    *messages.Repository
	chatIDs []int64
}

// New создаёт Service. creative — LLM для сценария/развязки (temp=0.9),
// det — для планов/разбора (temp=0).
func New(api *bot.Bot, creative, det *llm.LLMClient, repo *Repository, msgs *messages.Repository, chatIDs []int64) *Service {
	return &Service{api: api, gen: &Generator{creative: creative, det: det}, repo: repo, msgs: msgs, chatIDs: chatIDs}
}

// StartSession запускает смену в чате: сценарий раунда 1 → persist (сессия + раунд,
// фаза brief) → пост брифа. Активная смена уже есть → ErrActiveSession.
func (s *Service) StartSession(ctx context.Context, chatID int64) error {
	sess, err := s.repo.ActiveSession(ctx, chatID)
	if err != nil {
		return fmt.Errorf("oncall start: %w", err)
	}
	if sess != nil {
		return ErrActiveSession
	}
	ban, err := s.repo.RecentTitles(ctx, chatID, recentTitlesLimit)
	if err != nil {
		slog.Warn("oncall recent titles", "err", err)
		ban = nil
	}
	sc, err := s.gen.GenerateScenario(ctx, chatID, 1, "", ban)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("oncall marshal scenario: %w", err)
	}
	deadline := time.Now().Add(briefPhase)
	sessID, err := s.repo.CreateSession(ctx, chatID, sc.Title, string(raw), deadline)
	if err != nil {
		return err
	}
	if _, err := s.repo.InsertRound(ctx, sessID, chatID, 1, sc.Title, string(raw)); err != nil {
		return err
	}
	if err := s.postBrief(ctx, sessID, chatID, 1, sc, deadline); err != nil {
		return err
	}
	slog.Info("oncall session started", "chat_id", chatID, "session", sessID, "title", sc.Title)
	return nil
}

// HandleCallback обрабатывает голос за план: "oncall:<sid>:<round>:<planIdx>".
// Один голос на юзера на раунд; live-tally — редактированием голосовательного
// сообщения. Возвращает (текст тоста, showAlert).
func (s *Service) HandleCallback(ctx context.Context, cb *models.CallbackQuery) (string, bool) {
	parts := strings.Split(cb.Data, ":") // oncall:<sid>:<round>:<planIdx>
	if len(parts) != 4 {
		return "Некорректная кнопка", false
	}
	sid, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "Некорректная кнопка", false
	}
	round, err := strconv.Atoi(parts[2])
	if err != nil {
		return "Некорректная кнопка", false
	}
	idx, err := strconv.Atoi(parts[3])
	if err != nil {
		return "Некорректная кнопка", false
	}
	sess, err := s.repo.GetSession(ctx, sid)
	if err != nil {
		return "Ошибка, попробуй ещё", false
	}
	if sess == nil || sess.Status != statusActive {
		return "Смена уже не активна", false
	}
	if !voteWindowOpen(sess, time.Now()) {
		return "Голосование закрыто", false
	}
	if round != sess.Round {
		return "Это уже прошедший раунд", false
	}
	row, err := s.repo.RoundByNumber(ctx, sid, round)
	if err != nil || row == nil {
		return "Раунд не найден", false
	}
	plans := row.ParsedPlans()
	if idx < 0 || idx >= len(plans) {
		return "Некорректная кнопка", false
	}
	isNew, err := s.repo.RecordBallot(ctx, sid, round, cb.From.ID, idx)
	if err != nil {
		return "Ошибка, попробуй ещё", false
	}
	if !isNew {
		return "Ты уже голосовал 🙂", false
	}
	counts, cerr := s.repo.BallotCounts(ctx, sid, round)
	if cerr != nil {
		slog.Warn("oncall ballot counts", "err", cerr)
		return "Голос учтён", false
	}
	if sess.PhaseMessageID != 0 {
		kb := voteKeyboard(sid, round, plans, counts)
		if _, eerr := s.api.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      sess.ChatID,
			MessageID:   int(sess.PhaseMessageID),
			Text:        md.ToHTML(capPost(renderVote(round, maxRounds, plans, counts, sess.PhaseDeadline))),
			ParseMode:   models.ParseModeHTML,
			ReplyMarkup: &kb,
		}); eerr != nil {
			slog.Warn("oncall tally edit", "err", eerr)
		}
	}
	return "Голос учтён", false
}

// Cancel — админ-отмена активной смены: закрытие в БД + пост «Смена отменена».
func (s *Service) Cancel(ctx context.Context, chatID int64) error {
	sess, err := s.repo.ActiveSession(ctx, chatID)
	if err != nil {
		return fmt.Errorf("oncall cancel: %w", err)
	}
	if sess == nil {
		return ErrNoSession
	}
	if err := s.repo.Finish(ctx, sess.ID, "cancelled"); err != nil {
		return err
	}
	if _, err := s.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: md.ToHTML(renderCancelled()), ParseMode: models.ParseModeHTML,
	}); err != nil {
		slog.Warn("oncall cancel post", "err", err)
	}
	slog.Info("oncall session cancelled", "chat_id", chatID, "session", sess.ID)
	return nil
}

// --- постинг (persist-then-post: контент уже в БД, сюда приходит id сообщения) ---

// sendHTML постит текст фазы: первая попытка HTML (md.ToHTML), при ошибке отправки —
// вторая тем же текстом без ParseMode (plain, паттерн tg.SendMessage). Обе неудачны →
// счётчик post_attempts; при postFailLimit неудачах сессия закрывается (постинг
// недоступен навсегда — вечный resume-ретрай тика не нужен). Успех → привязка id
// сообщения к фазе и сброс счётчика.
func (s *Service) sendHTML(ctx context.Context, sessID, chatID int64, text string, kb *models.InlineKeyboardMarkup) error {
	html := &bot.SendMessageParams{ChatID: chatID, Text: md.ToHTML(text), ParseMode: models.ParseModeHTML}
	plain := &bot.SendMessageParams{ChatID: chatID, Text: text}
	if kb != nil { // typed-nil указатель в interface-поле = не-nil интерфейс → reply_markup:null → 400
		html.ReplyMarkup = kb
		plain.ReplyMarkup = kb
	}
	sent, err := s.api.SendMessage(ctx, html)
	if err != nil {
		slog.Warn("oncall send html failed, retry plain", "err", err, "chat_id", chatID)
		sent, err = s.api.SendMessage(ctx, plain)
	}
	if err != nil {
		n, cerr := s.repo.PostFailed(ctx, sessID)
		if cerr != nil {
			slog.Warn("oncall post attempts count", "err", cerr, "session", sessID)
			return fmt.Errorf("oncall send: %w", err)
		}
		if shouldFinishAfterPostFail(n) {
			if ferr := s.repo.Finish(ctx, sessID, "posting_unavailable"); ferr != nil {
				slog.Warn("oncall finish after post failures", "err", ferr, "session", sessID)
			}
			slog.Error("oncall session finished: posting unavailable", "session", sessID, "attempts", n, "chat_id", chatID)
		}
		return fmt.Errorf("oncall send: %w", err)
	}
	if sent != nil {
		if err := s.repo.SetPhaseMessage(ctx, sessID, int64(sent.ID)); err != nil {
			slog.Warn("oncall set phase message", "err", err)
		}
	}
	return nil
}

// shouldFinishAfterPostFail — чистая: пора ли закрывать сессию по счётчику неудачных
// постов (порог postFailLimit).
func shouldFinishAfterPostFail(attempts int) bool { return attempts >= postFailLimit }

// voteWindowOpen — чистая: голосование раунда ещё открыто (фаза vote и дедлайн фазы
// не истёк — поздние клики по старым кнопкам не учитываются; граница совпадает с
// stepVote: при now >= deadline голосование уже закрыто).
func voteWindowOpen(sess *SessionRow, now time.Time) bool {
	return sess.Phase == phaseVote && now.Before(sess.PhaseDeadline)
}

// postBrief постит бриф раунда и привязывает id сообщения к фазе.
func (s *Service) postBrief(ctx context.Context, sessID, chatID int64, round int, sc *Scenario, deadline time.Time) error {
	return s.sendHTML(ctx, sessID, chatID, capPost(renderBrief(round, maxRounds, sc, deadline)), nil)
}

// postVote постит голосование с кнопками планов и привязывает id сообщения.
func (s *Service) postVote(ctx context.Context, sessID, chatID int64, round int, plans []Plan, counts map[int]int, deadline time.Time) error {
	if counts == nil {
		counts = map[int]int{}
	}
	kb := voteKeyboard(sessID, round, plans, counts)
	return s.sendHTML(ctx, sessID, chatID, capPost(renderVote(round, maxRounds, plans, counts, deadline)), &kb)
}

// postPlain постит текст фазы (развязка/разбор) и привязывает id сообщения.
func (s *Service) postPlain(ctx context.Context, sessID, chatID int64, text string) error {
	return s.sendHTML(ctx, sessID, chatID, capPost(text), nil)
}

// --- чистые функции рендера ---

// renderBrief — пост сценария раунда: шапка, severity, вводная, метрики, инструкция.
func renderBrief(round, total int, sc *Scenario, deadline time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚨 ДЕЖУРСТВО — РАУНД %d/%d\n", round, total)
	fmt.Fprintf(&b, "%s · %s\n\n", severityLabel(sc.Severity), strings.TrimSpace(sc.Title))
	fmt.Fprintf(&b, "%s\n", strings.TrimSpace(sc.Narrative))
	if m := strings.TrimSpace(sc.Metrics); m != "" {
		fmt.Fprintf(&b, "\n📊 Метрики:\n%s\n", m)
	}
	fmt.Fprintf(&b, "\n💬 Пиши свой план действий сообщением в чат до %s. Предложения уйдут в голосование.", deadline.Format("15:04"))
	return b.String()
}

// renderVote — пост голосования: планы номерами (от консервативного к рискованному)
// и live-tally голосов, если они уже есть.
func renderVote(round, total int, plans []Plan, counts map[int]int, deadline time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🗳 ДЕЖУРСТВО — РАУНД %d/%d · ГОЛОСОВАНИЕ\n\n", round, total)
	for i, p := range plans {
		fmt.Fprintf(&b, "%d. **%s**\n%s\n", i+1, p.Title, p.Summary)
	}
	votes := 0
	for _, c := range counts {
		votes += c
	}
	fmt.Fprintf(&b, "\nГолосуй кнопкой до %s.", deadline.Format("15:04"))
	if votes > 0 {
		fmt.Fprintf(&b, " Голосов: %d.", votes)
	}
	b.WriteString("\nОдин голос на дежурного. При ничьей или тишине играем самый консервативный план.")
	return b.String()
}

// renderOutcome — развязка раунда: принятый план, что произошло, осложнение, метрики.
func renderOutcome(chosen Plan, out *Outcome, state string) string {
	var b strings.Builder
	b.WriteString("⚡️ ДЕЖУРСТВО — РАЗВЯЗКА РАУНДА\n\n")
	fmt.Fprintf(&b, "Принят план: **%s**\n%s\n\n", chosen.Title, chosen.Summary)
	fmt.Fprintf(&b, "Что произошло:\n%s\n", strings.TrimSpace(out.Text))
	if c := strings.TrimSpace(out.Complication); c != "" {
		fmt.Fprintf(&b, "\n⚠️ Осложнение: %s\n", c)
	}
	st := strings.TrimSpace(state)
	if st == "" {
		st = strings.TrimSpace(out.State)
	}
	if st != "" {
		fmt.Fprintf(&b, "\n📊 Метрики:\n%s\n", st)
	}
	return b.String()
}

// renderFinal — посмертный разбор: вердикт, итог, верно/ошибки, хронология раундов.
func renderFinal(pm *Postmortem, rounds []RoundRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🏁 ИСХОД: %s (%s)\n\n", verdictLabel(pm.Verdict), pm.Verdict)
	b.WriteString("📋 ПОСМЕРТНЫЙ РАЗБОР\n")
	if pm.Summary != "" {
		fmt.Fprintf(&b, "%s\n", pm.Summary)
	}
	if pm.Right != "" {
		fmt.Fprintf(&b, "\n✅ Верно:\n%s\n", pm.Right)
	}
	if pm.Wrong != "" {
		fmt.Fprintf(&b, "\n❌ Ошибки:\n%s\n", pm.Wrong)
	}
	if lines := roundLines(rounds); lines != "" {
		fmt.Fprintf(&b, "\n🧭 Хронология:\n%s", lines)
	}
	return b.String()
}

// renderSilentFinish — мягкое завершение при тишине: короткий статичный текст и,
// если LLM ответил, компактный разбор.
func renderSilentFinish(pm *Postmortem) string {
	var b strings.Builder
	b.WriteString("😴 ДЕЖУРСТВО — смена завершена досрочно\n\n")
	b.WriteString("В чате тихо: дежурные не вышли на связь. Смена завершена досрочно, инцидент передан следующей смене.\n")
	if pm != nil && pm.Summary != "" {
		fmt.Fprintf(&b, "\n📋 ПОСМЕРТНЫЙ РАЗБОР (%s)\n%s\n", verdictLabel(pm.Verdict), pm.Summary)
	}
	return b.String()
}

func renderCancelled() string {
	return "🚨 ДЕЖУРСТВО\n\nСмена отменена администратором. Инцидент передан в архив без разбора."
}

// voteKeyboard строит кнопки планов: по одной в ряд, с live-счётчиком голосов.
// callback_data: oncall:<sid>:<round>:<planIdx>.
func voteKeyboard(sessID int64, round int, plans []Plan, counts map[int]int) models.InlineKeyboardMarkup {
	prefix := "oncall:" + strconv.FormatInt(sessID, 10) + ":" + strconv.Itoa(round) + ":"
	rows := make([][]models.InlineKeyboardButton, 0, len(plans))
	for i, p := range plans {
		label := fmt.Sprintf("%d. %s", i+1, p.Title)
		if n := counts[i]; n > 0 {
			label += fmt.Sprintf(" (%d)", n)
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text: label, CallbackData: prefix + strconv.Itoa(i),
		}})
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func severityLabel(sev string) string {
	switch sev {
	case sev1:
		return "⚠️ SEV1 · критичный"
	case sev2:
		return "⚠️ SEV2 · деградация"
	case sev3:
		return "SEV3 · локальный"
	}
	return "SEV? · неизвестный"
}

func verdictLabel(v string) string {
	switch v {
	case verdictClosed:
		return "инцидент закрыт"
	case verdictPartial:
		return "закрыт частично"
	case verdictFailed:
		return "провал смены"
	}
	return "вердикт неизвестен"
}

// roundLines — хронология раундов для финального поста.
func roundLines(rounds []RoundRow) string {
	var b strings.Builder
	for _, r := range rounds {
		plan := "план не выбран"
		if p := r.ParsedPlans(); r.ChosenPlan >= 0 && r.ChosenPlan < len(p) {
			plan = p[r.ChosenPlan].Title
		}
		fmt.Fprintf(&b, "Раунд %d. %s → «%s»\n", r.Round, r.Title, plan)
	}
	return b.String()
}

// capPost — предохранитель бюджета поста: усечение по UTF-16 code units
// (эмодзи = 2), с «…» на конце.
func capPost(s string) string {
	if md.UTF16Len(s) <= postMaxUTF16 {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		cost := md.UTF16Len(string(r))
		if n+cost+1 > postMaxUTF16 { // +1 — место под «…»
			break
		}
		b.WriteRune(r)
		n += cost
	}
	return b.String() + "…"
}
