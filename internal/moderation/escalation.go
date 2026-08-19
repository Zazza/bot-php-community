package moderation

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// spamVoteCooldown — минимальный интервал между голосами одного участника по флагам.
const spamVoteCooldown = 5 * time.Minute

// escCallbackPrefix — префикс callback_data кнопок эскалации: spamesc:<flagID>:<spam|ok|ban|restore>.
const escCallbackPrefix = "spamesc:"

// escVoteWindow — сколько живут кнопки голосования на посте-предупреждении.
const escVoteWindow = 24 * time.Hour

// EscalationConfig — настройки голосования по спам-предупреждению.
type EscalationConfig struct {
	Enabled       bool          // kill-switch: кнопки голосования и свипер переоценки
	VoterMsgs     int           // мин. сообщений участника, чтобы голос считался (0 = все голосуют)
	EscalateSpam  int           // голосов «спам» до эскалации
	EscalateOk    int           // голосов «не спам» до снятия предупреждения
	RestrictHours time.Duration // длительность рестрикта при эскалации
}

// SpamEscalation — голосование участников по посту-предупреждению: «спам»/«не спам».
// Порог «спам» → рестрикт автора + уведомление админов с кнопками бана/восстановления;
// порог «не спам» → снятие предупреждения (ложная тревога). Терминальные переходы
// атомарны (claim через UPDATE ... WHERE), side-effect выполняется ровно один раз.
type SpamEscalation struct {
	api       *bot.Bot
	repo      *Repository
	counter   messageCounter
	adminIDs  map[int64]struct{}
	botUserID int64
	cfg       EscalationConfig

	mu       sync.Mutex
	lastVote map[int64]time.Time

	stop chan struct{}
	wg   sync.WaitGroup
}

func NewSpamEscalation(api *bot.Bot, repo *Repository, counter messageCounter, adminIDs []int64, botUserID int64, cfg EscalationConfig) *SpamEscalation {
	amap := make(map[int64]struct{}, len(adminIDs))
	for _, id := range adminIDs {
		amap[id] = struct{}{}
	}
	return &SpamEscalation{
		api:       api,
		repo:      repo,
		counter:   counter,
		adminIDs:  amap,
		botUserID: botUserID,
		cfg:       cfg,
		lastVote:  make(map[int64]time.Time),
		stop:      make(chan struct{}),
	}
}

func (e *SpamEscalation) isAdmin(userID int64) bool {
	_, ok := e.adminIDs[userID]
	return ok
}

// HandleCallback обрабатывает клик по кнопкам поста-предупреждения (spamesc:) и
// admin-кнопкам из ЛС-уведомления. Возвращает текст toast.
func (e *SpamEscalation) HandleCallback(ctx context.Context, cb *models.CallbackQuery) string {
	flagID, verb, ok := parseEscalationCallback(cb.Data)
	if !ok {
		return "Некорректная кнопка"
	}
	if verb == "ban" || verb == "restore" {
		return e.handleAdminAction(ctx, cb, flagID, verb)
	}
	if !e.cfg.Enabled {
		return "Эскалация отключена"
	}

	flag, err := e.repo.GetSpamFlag(ctx, flagID)
	if err != nil {
		slog.Warn("get spam flag", "err", err)
		return "Предупреждение уже не активно"
	}
	if flag == nil {
		return "Предупреждение уже не активно"
	}
	if flag.EscalatedAt != nil || flag.FalsePositive || flag.AdminAction != nil {
		return "Уже решено"
	}
	if cb.From.ID == 0 {
		return "—"
	}
	if cb.From.ID == flag.TGUserID {
		return "Нельзя голосовать по своему предупреждению"
	}
	if cb.From.ID == e.botUserID {
		return "—"
	}
	if time.Since(flag.DetectedAt) > escVoteWindow {
		return "Голосование завершено"
	}

	voterID := cb.From.ID
	e.mu.Lock()
	if left := voteCooldownLeft(e.lastVote[voterID], time.Now()); left > 0 {
		e.mu.Unlock()
		return fmt.Sprintf("Слишком часто — подождите %v", left.Round(time.Second))
	}
	e.mu.Unlock()

	admin := e.isAdmin(voterID)
	if !admin && e.cfg.VoterMsgs > 0 && e.counter != nil {
		n, cerr := e.counter.CountByUser(ctx, flag.ChatID, voterID, e.cfg.VoterMsgs)
		if cerr != nil {
			slog.Warn("spam vote count", "err", cerr)
		} else if n < e.cfg.VoterMsgs {
			slog.Info("spam vote novice skipped", "user", voterID, "count", n)
			return "Голос новичков не считается"
		}
	}

	spamN, okN, dup, err := e.repo.CastSpamBallot(ctx, flagID, voterID, verb)
	if err != nil {
		slog.Warn("cast spam ballot", "err", err)
		return "Ошибка голосования"
	}
	if dup {
		return "Ты уже голосовал"
	}
	e.mu.Lock()
	e.lastVote[voterID] = time.Now()
	e.mu.Unlock()
	slog.Info("spam vote cast", "flag", flagID, "user", voterID, "choice", verb)

	if admin {
		if verb == "spam" {
			e.escalate(ctx, flag, spamN, e.cfg.EscalateSpam)
			return "Эскалация подтверждена админом — рестрикт"
		}
		e.clear(ctx, flag, okN, e.cfg.EscalateOk, true)
		return "Предупреждение снято админом"
	}

	switch e.resolveFlag(ctx, flag, spamN, okN, false) {
	case "escalate":
		return "Эскалация: рестрикт до решения админа"
	case "clear":
		return "Предупреждение снято — ложная тревога"
	}
	if verb == "spam" {
		return fmt.Sprintf("Принято: %d за спам / %d не спам (порог %d)", spamN, okN, e.cfg.EscalateSpam)
	}
	return fmt.Sprintf("Принято: %d за спам / %d не спам", spamN, okN)
}

// handleAdminAction — кнопки [⛔ Бан навсегда]/[↩️ Восстановить] из ЛС-уведомления.
func (e *SpamEscalation) handleAdminAction(ctx context.Context, cb *models.CallbackQuery, flagID int64, verb string) string {
	if cb.From.ID == 0 {
		return "—"
	}
	if !e.isAdmin(cb.From.ID) {
		return "Только админ"
	}
	flag, err := e.repo.GetSpamFlag(ctx, flagID)
	if err != nil {
		slog.Warn("get spam flag", "err", err)
		return "Предупреждение уже не активно"
	}
	if flag == nil {
		return "Предупреждение уже не активно"
	}
	if verb == "ban" {
		return e.banForever(ctx, flag)
	}
	return e.restore(ctx, flag)
}

// resolveFlag — общий вход решения по флагу: порог по счётчикам → escalate/clear.
// Счётчики передаются вызывающим: callback — свежие из RETURNING CastSpamBallot
// (снимок флага старше решающего голоса), свипер — из строки БД. Возвращает решение
// ("" — порог не достигнут). Свипер и голоса сообщества: adminClick=false.
func (e *SpamEscalation) resolveFlag(ctx context.Context, flag *SpamFlag, spamN, okN int, adminClick bool) string {
	switch decideEscalation(spamN, okN, e.cfg.EscalateSpam, e.cfg.EscalateOk) {
	case "escalate":
		e.escalate(ctx, flag, spamN, e.cfg.EscalateSpam)
		return "escalate"
	case "clear":
		e.clear(ctx, flag, okN, e.cfg.EscalateOk, adminClick)
		return "clear"
	}
	return ""
}

// escalate — рестрикт автора + правка поста + уведомление админов. Выполняется один раз
// (ClaimSpamEscalation), даже при одновременном достижении порога двумя голосами.
// Рестрикт ставится БЕЗУСЛОВНО (идемпотентно, свежий UntilDate): снимок флага мог
// устареть — рестрикт истёк или снят, а DB-срок продлевать мимо TG нельзя.
func (e *SpamEscalation) escalate(ctx context.Context, flag *SpamFlag, spamN, need int) {
	claimed, err := e.repo.ClaimSpamEscalation(ctx, flag.ID)
	if err != nil {
		slog.Warn("claim spam escalation", "err", err)
		return
	}
	if !claimed {
		return
	}
	fresh, err := e.repo.GetSpamFlag(ctx, flag.ID)
	if err != nil || fresh == nil {
		slog.Warn("reload spam flag after claim", "err", err)
		fresh = flag
	}
	until := time.Now().Add(e.cfg.RestrictHours)
	if err := restrictUserTextOnlyUntil(ctx, e.api, fresh.ChatID, fresh.TGUserID, until); err != nil {
		slog.Warn("spam escalate restrict", "err", err)
		_, _ = e.api.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: fresh.ChatID,
			Text: fmt.Sprintf("⚠️ Эскалация сработала, но рестрикт %s не удался — админ, примите меры.",
				atUser(fresh.Username, "участника")),
		})
	}
	if err := e.repo.SetSpamRestrict(ctx, fresh.ID, until); err != nil {
		slog.Warn("set spam restrict", "err", err)
	}
	e.editWarnMessage(ctx, fresh, fmt.Sprintf("🚫 Эскалация: %d/%d за спам — рестрикт %d ч (только текст).",
		spamN, need, int(e.cfg.RestrictHours.Hours())))
	e.notifyAdmins(ctx, fresh, spamN, need)
	slog.Info("spam escalated", "flag", fresh.ID, "user", fresh.TGUserID, "spam", spamN)
}

// clear — снятие предупреждения по «не спам»: false_positive-пометка + правка поста.
// Рестрикт снимается только по shouldReleaseRestrict: голоса сообщества отменяют
// собственную эскалацию, но НЕ авторестрикт по счётчику WarnMax (action='restrict') —
// для него нужен админ; рестрикт истечёт сам по UntilDate.
func (e *SpamEscalation) clear(ctx context.Context, flag *SpamFlag, okN, need int, adminClick bool) {
	claimed, err := e.repo.ClaimSpamFalsePositive(ctx, flag.ID)
	if err != nil {
		slog.Warn("claim spam false positive", "err", err)
		return
	}
	if !claimed {
		return
	}
	release := flag.RestrictUntil != nil && flag.ReleasedAt == nil &&
		shouldReleaseRestrict(flag.Action, adminClick, flag.EscalatedAt != nil)
	keptRestrict := flag.Action == "restrict" && flag.RestrictUntil != nil && flag.ReleasedAt == nil && !release
	if release {
		if err := unmuteUserFull(ctx, e.api, flag.ChatID, flag.TGUserID); err != nil {
			slog.Warn("spam false positive unmute", "err", err)
			_, _ = e.api.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: flag.ChatID,
				Text: fmt.Sprintf("⚠️ Снятие рестрикта с %s не удалось (ошибка TG) — снимите вручную.",
					atUser(flag.Username, "участника")),
			})
			return
		}
		if err := e.repo.ReleaseSpamRestrict(ctx, flag.ID); err != nil {
			slog.Warn("release spam restrict", "err", err)
		}
	}
	if keptRestrict {
		e.editWarnMessage(ctx, flag, "✅ Предупреждение снято — ложная тревога. Рестрикт истечёт сам, админ может снять вручную.")
		slog.Warn("spam false positive: authoristrict kept, admin required", "flag", flag.ID, "user", flag.TGUserID)
	} else {
		e.editWarnMessage(ctx, flag, fmt.Sprintf("✅ Предупреждение снято — ложная тревога (%d/%d «не спам»).",
			okN, need))
	}
	slog.Info("spam false positive", "flag", flag.ID, "user", flag.TGUserID, "ok", okN)
}

// banForever — решение админа после эскалации: необратимый бан. При сбое TG claim
// откатывается (ResetSpamAdminAction), путь остаётся открытым для повторного клика.
func (e *SpamEscalation) banForever(ctx context.Context, flag *SpamFlag) string {
	claimed, err := e.repo.ClaimSpamAdminAction(ctx, flag.ID, "banned")
	if err != nil {
		slog.Warn("claim spam admin ban", "err", err)
		return "Ошибка"
	}
	if !claimed {
		return "Уже решено"
	}
	if err := banUserForever(ctx, e.api, flag.ChatID, flag.TGUserID); err != nil {
		slog.Warn("spam ban forever", "err", err)
		return e.failAdminAction(ctx, flag, "banned",
			fmt.Sprintf("⚠️ Бан %s не удался (ошибка TG) — повторите клик или забаньте вручную.",
				atUser(flag.Username, "участника")))
	}
	_, _ = e.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: flag.ChatID,
		Text: fmt.Sprintf("⛔ %s забанен админом за спам.",
			atUser(flag.Username, "участник")),
	})
	slog.Info("spam banned by admin", "flag", flag.ID, "user", flag.TGUserID)
	return "Забанен навсегда"
}

// restore — решение админа после эскалации: снять рестрикт, размьютить. Разрешён и
// поверх ошибочного бана (admin_action='banned'). unbanIfBanned вызывается БЕЗУСЛОВНО
// (OnlyIfBanned → no-op, если бана нет): wasBanned из снимка до claim мог устареть —
// параллельный бан другого админа обязан закрыться разбаном.
func (e *SpamEscalation) restore(ctx context.Context, flag *SpamFlag) string {
	wasBanned := bannedAction(flag.AdminAction)
	claimed, err := e.repo.ClaimSpamAdminAction(ctx, flag.ID, "restored")
	if err != nil {
		slog.Warn("claim spam admin restore", "err", err)
		return "Ошибка"
	}
	if !claimed {
		return "Уже решено"
	}
	if err := unbanIfBanned(ctx, e.api, flag.ChatID, flag.TGUserID); err != nil {
		slog.Warn("spam restore unban", "err", err)
		return e.failAdminAction(ctx, flag, "restored",
			fmt.Sprintf("⚠️ Восстановление %s не удалось (ошибка TG) — повторите клик или снимите бан вручную.",
				atUser(flag.Username, "участника")))
	}
	if err := unmuteUserFull(ctx, e.api, flag.ChatID, flag.TGUserID); err != nil {
		slog.Warn("spam restore unmute", "err", err)
		return e.failAdminAction(ctx, flag, "restored",
			fmt.Sprintf("⚠️ Снятие рестрикта с %s не удалось (ошибка TG) — повторите клик или снимите вручную.",
				atUser(flag.Username, "участника")))
	}
	if err := e.repo.ReleaseSpamRestrict(ctx, flag.ID); err != nil {
		slog.Warn("release spam restrict", "err", err)
	}
	_, _ = e.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: flag.ChatID,
		Text: fmt.Sprintf("↩️ Рестрикт с %s снят админом.",
			atUser(flag.Username, "участника")),
	})
	slog.Info("spam restored by admin", "flag", flag.ID, "user", flag.TGUserID, "after_ban", wasBanned)
	return "Рестрикт снят"
}

// failAdminAction — TG-вызов решения админа упал: откатить claim (кнопка останется
// рабочей), честно сообщить в чат флага, попросить повторить клик.
func (e *SpamEscalation) failAdminAction(ctx context.Context, flag *SpamFlag, action, text string) string {
	if err := e.repo.ResetSpamAdminAction(ctx, flag.ID, action); err != nil {
		slog.Warn("reset spam admin action", "err", err)
	}
	_, _ = e.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: flag.ChatID,
		Text:   text,
	})
	return "Ошибка, попробуйте ещё"
}

// notifyAdmins — ЛС каждому админу с кнопками бана/восстановления и списком голосовавших.
// Бот не может писать первым (403, если админ не сделал /start): все ДМ упали → один
// fallback-пост в чат флага — БЕЗ строки голосовавших (не деанонимизируем голосующих
// перед автором); клавиатура остаётся: посторонним клик даёт «Только админ».
func (e *SpamEscalation) notifyAdmins(ctx context.Context, flag *SpamFlag, spamN, need int) {
	voters, err := e.repo.SpamVoters(ctx, flag.ID)
	if err != nil {
		slog.Warn("spam voters", "err", err)
	}
	names := make([]string, 0, len(voters))
	for _, v := range voters {
		if v.Username != "" {
			names = append(names, "@"+v.Username)
		} else {
			names = append(names, strconv.FormatInt(v.UserID, 10))
		}
	}
	voterLine := strings.Join(names, ", ")
	if voterLine == "" {
		voterLine = "—"
	}
	reason := truncateReason(sanitizeReason(flag.Reason))
	dmText := fmt.Sprintf("🚨 %s получил %d/%d голосов за спам: %s\nГолосовали: %s",
		atUser(flag.Username, "участник"), spamN, need, reason, voterLine)
	chatText := fmt.Sprintf("🚨 %s получил %d/%d голосов за спам: %s",
		atUser(flag.Username, "участник"), spamN, need, reason)
	kb := adminKeyboard(flag.ID)

	delivered := false
	for id := range e.adminIDs {
		if _, err := e.api.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: id, Text: dmText, ReplyMarkup: kb,
		}); err != nil {
			slog.Warn("spam escalate notify admin", "err", err, "admin_id", id)
			continue
		}
		delivered = true
	}
	if !delivered {
		_, _ = e.api.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: flag.ChatID, Text: chatText, ReplyMarkup: kb,
		})
		slog.Warn("spam escalate notify fallback to chat", "chat", flag.ChatID)
	}
}

func (e *SpamEscalation) editWarnMessage(ctx context.Context, flag *SpamFlag, text string) {
	if flag.WarnMessageID == 0 {
		return
	}
	_, err := e.api.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:      flag.ChatID,
		MessageID:   int(flag.WarnMessageID),
		Text:        text,
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}},
	})
	if err != nil {
		slog.Warn("edit warn message", "err", err)
	}
}

// Start запускает свипер переоценки (30s). Kill-switch: Enabled=false → выход.
func (e *SpamEscalation) Start(ctx context.Context) {
	if !e.cfg.Enabled {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.stop:
				return
			case <-t.C:
				e.sweep(ctx)
			}
		}
	}()
	slog.Info("spam escalation sweeper started")
}

func (e *SpamEscalation) Stop() {
	close(e.stop)
	e.wg.Wait()
}

// sweep — доводит зависшие флаги до решения: крэш между CastSpamBallot и claim
// оставил бы достигнутый порог без escalate/clear. Окно как у голосования (24ч).
func (e *SpamEscalation) sweep(ctx context.Context) {
	pending, err := e.repo.SpamFlagsPendingDecision(ctx, time.Now(), e.cfg.EscalateSpam, e.cfg.EscalateOk, escVoteWindow)
	if err != nil {
		slog.Warn("spam flags pending decision", "err", err)
		return
	}
	for i := range pending {
		e.resolveFlag(ctx, &pending[i], pending[i].SpamCount, pending[i].OkCount, false)
	}
}

// shouldReleaseRestrict — можно ли снять TG-рестрикт при снятии предупреждения.
// Админский клик — всегда да (админ всемогущ). Голоса сообщества снимают только
// рестрикт собственной эскалации (escalated_at); авторестрикт по счётчику WarnMax
// (action='restrict') без админа НЕ снимается — 2 голоса «не спам» не размьючивают рецидивиста.
func shouldReleaseRestrict(action string, adminClick, escalated bool) bool {
	if adminClick || escalated {
		return true
	}
	return action != "restrict"
}

// bannedAction — флаг уже в состоянии ошибочного бана (restore поверх banned).
func bannedAction(a *string) bool {
	return a != nil && *a == "banned"
}

// decideEscalation — "escalate" при spamN>=escSpam, "clear" при okN>=escOk;
// оба порога достигнуты → "escalate" (зеркало decideOutcome: приоритет более строгому исходу).
func decideEscalation(spamN, okN, escSpam, escOk int) string {
	if spamN >= escSpam {
		return "escalate"
	}
	if okN >= escOk {
		return "clear"
	}
	return ""
}

// voteCooldownLeft — сколько осталось ждать до следующего голоса (0 — можно голосовать).
func voteCooldownLeft(last, now time.Time) time.Duration {
	if last.IsZero() {
		return 0
	}
	if left := spamVoteCooldown - now.Sub(last); left > 0 {
		return left
	}
	return 0
}

// parseEscalationCallback разбирает callback_data spamesc:<flagID>:<verb>.
func parseEscalationCallback(data string) (flagID int64, verb string, ok bool) {
	if !strings.HasPrefix(data, escCallbackPrefix) {
		return 0, "", false
	}
	parts := strings.Split(data, ":") // spamesc:<id>:<verb>
	if len(parts) != 3 {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, "", false
	}
	switch parts[2] {
	case "spam", "ok", "ban", "restore":
		return id, parts[2], true
	}
	return 0, "", false
}

// spamKeyboard — кнопки голосования на посте-предупреждении.
func spamKeyboard(flagID int64) models.InlineKeyboardMarkup {
	prefix := escCallbackPrefix + strconv.FormatInt(flagID, 10) + ":"
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{
			{Text: "🗑 Спам", CallbackData: prefix + "spam"},
			{Text: "✅ Не спам", CallbackData: prefix + "ok"},
		},
	}}
}

// adminKeyboard — кнопки решения админа в ЛС-уведомлении об эскалации.
func adminKeyboard(flagID int64) models.InlineKeyboardMarkup {
	prefix := escCallbackPrefix + strconv.FormatInt(flagID, 10) + ":"
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{
			{Text: "⛔ Бан навсегда", CallbackData: prefix + "ban"},
			{Text: "↩️ Восстановить", CallbackData: prefix + "restore"},
		},
	}}
}
