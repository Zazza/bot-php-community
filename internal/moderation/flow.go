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
	"phpbot/internal/llm"
	"phpbot/internal/users"
)

// Flow оркестрирует диалог с новичком: при заходе шлёт вопрос, ждёт ответ в чате,
// зовёт судью, постит вердикт с inline-кнопками. Не кикает сам — только рекомендует.
type Flow struct {
	api       *bot.Bot
	llm       *llm.LLMClient
	repo      *Repository
	users     *users.Repository
	timeout   time.Duration
	adminIDs  map[int64]struct{}

	// pending: tg_user_id → отмена таймера ожидания ответа.
	pending map[int64]context.CancelFunc
	mu      sync.Mutex
}

// NewFlow создаёт Flow.
func NewFlow(api *bot.Bot, llm *llm.LLMClient, repo *Repository, usersRepo *users.Repository,
	timeout time.Duration, adminIDs []int64) *Flow {
	amap := make(map[int64]struct{}, len(adminIDs))
	for _, id := range adminIDs {
		amap[id] = struct{}{}
	}
	return &Flow{
		api:      api,
		llm:      llm,
		repo:     repo,
		users:    usersRepo,
		timeout:  timeout,
		adminIDs: amap,
		pending:  make(map[int64]context.CancelFunc),
	}
}

// OnNewMember шлёт приветствие + вопрос и ставит таймер ожидания.
func (f *Flow) OnNewMember(ctx context.Context, chatID, userID int64, username string) {
	display := "@" + username
	if username == "" {
		display = "новичок"
	}
	// Помечаем suspect до прохождения проверки.
	if _, err := f.users.Upsert(ctx, &users.User{
		TGUserID: userID, Username: username, Status: "suspect",
	}); err != nil {
		slog.Error("upsert suspect", "err", err)
	}

	text := fmt.Sprintf("Привет, %s! 👋\n\n%s\n\n(Бот присмотрится к ответу — если не ответишь за %s, администратор глянет вручную.)",
		display, Question, formatDuration(f.timeout))
	if _, err := f.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: text,
	}); err != nil {
		slog.Error("send welcome", "err", err)
		return
	}

	// Запускаем таймер: через timeout ставим unclear, если не ответит.
	tctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	f.setPending(userID, cancel)
	go f.awaitTimeout(tctx, chatID, userID, username)
}

// HandleAnswer проверяет сообщение: если автор в pending — это ответ новичка.
// Возвращает true, если сообщение поглощено модерацией (его не нужно дальше обрабатывать).
func (f *Flow) HandleAnswer(ctx context.Context, chatID, userID int64, username, text string) bool {
	cancel, ok := f.takePending(userID)
	if !ok {
		return false
	}
	cancel() // снимаем таймер

	verdict := Judge(ctx, f.llm, text)
	rec := &VerdictRecord{
		TGUserID: userID, ChatID: chatID,
		Question: Question, Answer: text,
		Verdict: verdict.Verdict, Reason: verdict.Reason,
	}
	id, err := f.repo.SaveVerdict(ctx, rec)
	if err != nil {
		slog.Error("save verdict", "err", err)
	}

	// Статус: human → member, иначе остаётся suspect.
	if verdict.Verdict == "human" {
		_ = f.users.SetStatus(ctx, userID, "member")
	} else {
		_ = f.users.SetStatus(ctx, userID, "suspect")
	}

	display := "@" + username
	if username == "" {
		display = "новичок"
	}
	post := fmt.Sprintf("%s %s — вердикт судьи: %s\nПричина: %s",
		VerdictEmoji(verdict.Verdict), display, verdict.Verdict, verdict.Reason)

	kb := buildKeyboard(id)
	if _, err := f.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: post, ReplyMarkup: kb,
	}); err != nil {
		slog.Error("post verdict", "err", err)
	}
	return true
}

// awaitTimeout срабатывает, если новичок не ответил за timeout.
func (f *Flow) awaitTimeout(ctx context.Context, chatID, userID int64, username string) {
	<-ctx.Done()
	if ctx.Err() == context.Canceled {
		return // ответ пришёл раньше
	}
	// Таймаут истёк — помечаем unclear и просим админа глянуть.
	f.clearPending(userID)
	rec := &VerdictRecord{
		TGUserID: userID, ChatID: chatID,
		Question: Question, Answer: "(нет ответа)",
		Verdict: "unclear", Reason: "Нет ответа за отведённое время",
	}
	id, err := f.repo.SaveVerdict(context.Background(), rec)
	if err != nil {
		slog.Error("save timeout verdict", "err", err)
	}
	display := "@" + username
	if username == "" {
		display = "новичок"
	}
	post := fmt.Sprintf("⏰ %s не ответил за %s — помечаю suspect, гляньте вручную.",
		display, formatDuration(f.timeout))
	if _, err := f.api.SendMessage(context.Background(), &bot.SendMessageParams{
		ChatID: chatID, Text: post, ReplyMarkup: buildKeyboard(id),
	}); err != nil {
		slog.Error("post timeout verdict", "err", err)
	}
}

// HandleCallback — нажатие на inline-кнопку [Кикнуть]/[Оставить]/[Спросить ещё].
// Возвращает текст ответа на callback_query (alert).
func (f *Flow) HandleCallback(ctx context.Context, cb *models.CallbackQuery) (string, bool) {
	if cb.From.ID == 0 {
		return "error: нет пользователя", false
	}
	if _, ok := f.adminIDs[cb.From.ID]; !ok {
		return "Только администратор может нажимать эти кнопки.", true
	}
	data := cb.Data
	parts := strings.Split(data, ":")
	if len(parts) != 2 {
		return "Некорректный callback", true
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "Некорректный id", true
	}
	rec, err := f.repo.Get(ctx, id)
	if err != nil {
		return "Вердикт не найден", true
	}

	switch parts[0] {
	case "kick":
		if err := f.kickUser(ctx, rec.ChatID, rec.TGUserID); err != nil {
			slog.Error("kick user", "err", err)
			return fmt.Sprintf("Ошибка кика: %v", err), true
		}
		_ = f.repo.SetAdminAction(ctx, id, "kicked")
		_ = f.users.MarkBanned(ctx, rec.TGUserID)
		return "Пользователь кикнут.", true
	case "keep":
		_ = f.repo.SetAdminAction(ctx, id, "kept")
		_ = f.users.SetStatus(ctx, rec.TGUserID, "member")
		return "Оставляем, пользователь отмечен как member.", true
	case "again":
		// Сбрасываем в suspect и снова ждём ответа (новый таймер).
		_ = f.users.SetStatus(ctx, rec.TGUserID, "suspect")
		f.OnNewMember(ctx, rec.ChatID, rec.TGUserID, rec.TGUserIDtoUsername())
		return "Спрошу ещё раз.", false
	default:
		return "Неизвестное действие", true
	}
}

// kickUser вызывает kickChatMember. Только рекомендация, фактический кик — после
// подтверждения админом через inline-кнопку.
func (f *Flow) kickUser(ctx context.Context, chatID, userID int64) error {
	_, err := f.api.BanChatMember(ctx, &bot.BanChatMemberParams{
		ChatID: chatID, UserID: userID,
	})
	if err != nil {
		return err
	}
	// Сразу разбаниваем, чтобы пользователь мог вернуться сам (kick, а не бан).
	_, _ = f.api.UnbanChatMember(ctx, &bot.UnbanChatMemberParams{
		ChatID: chatID, UserID: userID, OnlyIfBanned: true,
	})
	return nil
}

// ManualCheck — ручная judge-проверка пользователя (команда /check @user).
func (f *Flow) ManualCheck(ctx context.Context, chatID, userID int64, username, text string) Verdict {
	v := Judge(ctx, f.llm, text)
	_, _ = f.repo.SaveVerdict(ctx, &VerdictRecord{
		TGUserID: userID, ChatID: chatID,
		Question: "(manual /check)", Answer: text,
		Verdict: v.Verdict, Reason: v.Reason,
	})
	return v
}

// IsAdmin проверяет, является ли userID админом.
func (f *Flow) IsAdmin(userID int64) bool {
	_, ok := f.adminIDs[userID]
	return ok
}

// AdminIDs возвращает список id админов (для шифровки доступности команд).
func (f *Flow) AdminIDs() []int64 {
	out := make([]int64, 0, len(f.adminIDs))
	for id := range f.adminIDs {
		out = append(out, id)
	}
	return out
}

// --- helpers ---

func (f *Flow) setPending(userID int64, cancel context.CancelFunc) {
	f.mu.Lock()
	// Если уже был pending — отменяем старый.
	if old, ok := f.pending[userID]; ok {
		old()
	}
	f.pending[userID] = cancel
	f.mu.Unlock()
}

func (f *Flow) takePending(userID int64) (context.CancelFunc, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.pending[userID]
	delete(f.pending, userID)
	return c, ok
}

func (f *Flow) clearPending(userID int64) {
	f.mu.Lock()
	delete(f.pending, userID)
	f.mu.Unlock()
}

// buildKeyboard — inline-кнопки под постом вердикта. callback_data формата "<action>:<verdict_id>".
func buildKeyboard(verdictID int64) models.InlineKeyboardMarkup {
	prefix := strconv.FormatInt(verdictID, 10)
	return models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "🤖 Кикнуть", CallbackData: "kick:" + prefix},
			{Text: "✅ Оставить", CallbackData: "keep:" + prefix},
			{Text: "❓ Спросить ещё", CallbackData: "again:" + prefix},
		}},
	}
}

// formatDuration — человекочитаемо для русского (минуты).
func formatDuration(d time.Duration) string {
	min := int(d.Minutes())
	if min == 0 {
		return strconv.Itoa(int(d.Seconds())) + " сек"
	}
	return strconv.Itoa(min) + " мин"
}

// TGUserIDtoUsername — вспомогательный хелпер для VerdictRecord (без username в схеме).
// Используется только при re-ask, когда нет текущего username; в реальном чате он будет в сообщении.
func (r *VerdictRecord) TGUserIDtoUsername() string {
	return strconv.FormatInt(r.TGUserID, 10)
}
