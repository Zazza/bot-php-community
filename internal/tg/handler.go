package tg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"phpbot/internal/chat"
	"phpbot/internal/messages"
	"phpbot/internal/moderation"
	"phpbot/internal/topics"
	"phpbot/internal/users"
)

// replyTimeout — сколько ждём LLM/дайджеста в фоне.
const replyTimeout = 90 * time.Second

// Handlers — центральная диспетчеризация update. Содержит ссылки на все домены.
type Handlers struct {
	api          *bot.Bot
	chatIDs      map[int64]struct{}
	botUserID    int64  // собственный id бота, чтобы отличать reply-to-bot
	botUsername  string // ник бота для @упоминаний

	moderation *moderation.Flow
	users      *users.Repository
	msgs       *messages.Repository
	vec        *messages.VectorRepo
	answerer   *chat.Answerer
	topics     *topics.Scheduler
	digester   *topics.Digester
}

// HandlersDeps — зависимости для сборки Handlers.
type HandlersDeps struct {
	API        *bot.Bot
	ChatIDs    []int64
	BotUserID  int64
	Moderation *moderation.Flow
	Users      *users.Repository
	Msgs       *messages.Repository
	Vec        *messages.VectorRepo
	Answerer   *chat.Answerer
	Topics     *topics.Scheduler
	Digester   *topics.Digester
}

// NewHandlers собирает Handlers.
func NewHandlers(d HandlersDeps) *Handlers {
	h := &Handlers{
		api:        d.API,
		botUserID:  d.BotUserID,
		moderation: d.Moderation,
		users:      d.Users,
		msgs:       d.Msgs,
		vec:        d.Vec,
		answerer:   d.Answerer,
		topics:     d.Topics,
		digester:   d.Digester,
		chatIDs:    make(map[int64]struct{}, len(d.ChatIDs)),
	}
	for _, id := range d.ChatIDs {
		h.chatIDs[id] = struct{}{}
	}
	return h
}

// OnMessage — единый вход для всех message-update.
func (h *Handlers) OnMessage(ctx context.Context, b *bot.Bot, upd *models.Update) {
	msg := upd.Message
	if msg == nil {
		return
	}
	chatID := msg.Chat.ID

	// 1. new_chat_members — модерация новичков.
	if len(msg.NewChatMembers) > 0 {
		for _, u := range msg.NewChatMembers {
			if u.IsBot {
				continue
			}
			h.moderation.OnNewMember(ctx, chatID, u.ID, u.Username)
		}
		return
	}

	// Игнорируем сообщения не из отслеживаемых чатов.
	if _, ok := h.chatIDs[chatID]; !ok {
		return
	}

	text := msg.Text
	if msg.Caption != "" && text == "" {
		text = msg.Caption
	}

	// 2. Команды.
	if cmd, args := extractCommand(text); cmd != "" {
		h.dispatchCommand(ctx, chatID, msg, cmd, args)
		return
	}

	// 3. Сохраняем любое текстовое сообщение в историю + async embedding.
	if text != "" && msg.From != nil {
		h.saveMessage(ctx, msg, text)

		// 3a. Ответ новичка на вопрос модерации (если он в pending).
		if h.moderation.HandleAnswer(ctx, chatID, msg.From.ID, msg.From.Username, text) {
			return // сообщение поглощено модерацией
		}

		// 3b. PHP-триггеры: @упоминание бота или reply на сообщение бота.
		if h.isAddressedToBot(msg, text) {
			h.answerChat(ctx, chatID, msg, stripBotMention(text, h.botUserID))
		}
	}
}

// OnCallbackQuery — кнопки модерации.
func (h *Handlers) OnCallbackQuery(ctx context.Context, b *bot.Bot, upd *models.Update) {
	cb := upd.CallbackQuery
	if cb == nil {
		return
	}
	alert, _ := h.moderation.HandleCallback(ctx, cb)
	if alert != "" {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cb.ID, Text: alert, ShowAlert: true,
		})
	}
	// Снимаем «часики» с кнопки.
	if cb.Message.Message != nil {
		_, _ = b.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
			ChatID:    cb.Message.Message.Chat.ID,
			MessageID: cb.Message.Message.ID,
		})
	}
}

// dispatchCommand маршрутизирует slash-команды.
func (h *Handlers) dispatchCommand(ctx context.Context, chatID int64, msg *models.Message, cmd, args string) {
	switch cmd {
	case "ask":
		h.answerChat(ctx, chatID, msg, args)
	case "search":
		h.cmdSearch(ctx, chatID, args)
	case "help", "start":
		h.cmdHelp(ctx, chatID)
	case "stats":
		h.cmdStats(ctx, chatID)
	case "topic":
		h.cmdTopic(ctx, chatID, msg, args)
	case "digest":
		h.cmdDigest(ctx, chatID, msg, args)
	case "check":
		h.cmdCheck(ctx, chatID, msg, args)
	case "kick":
		h.cmdKick(ctx, chatID, msg, args)
	default:
		slog.Debug("unknown command", "cmd", cmd)
	}
}

// --- команды ---

func (h *Handlers) cmdHelp(ctx context.Context, chatID int64) {
	text := `*Команды бота*

/ask <вопрос> — ответ на PHP/IT-вопрос
/search <запрос> — поиск по истории чата
/stats — статистика чата
/help — этот текст

*Только для админов:*
/topic now — сгенерировать и запостить тему
/digest week — дайджест за неделю
/check @user — ручная judge-проверка последних сообщений
/kick @user — кик пользователя

Бот также отвечает на @упоминание и reply на своё сообщение.`
	_ = SendMessage(ctx, h.api, chatID, text)
}

func (h *Handlers) cmdStats(ctx context.Context, chatID int64) {
	s, err := h.msgs.Stats(ctx, chatID)
	if err != nil {
		_ = SendMessage(ctx, h.api, chatID, "Не удалось собрать статистику.")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📊 *Статистика чата*\n\nВсего сообщений: %d\nАктивных участников: %d\nЗа 24ч: %d\n\n*Топ за неделю:*",
		s.TotalMessages, s.ActiveUsers, s.LastDay)
	for i, p := range s.TopPosters {
		fmt.Fprintf(&b, "\n%d. %s — %d", i+1, p.Username, p.Count)
	}
	_ = SendMessage(ctx, h.api, chatID, b.String())
}

func (h *Handlers) cmdSearch(ctx context.Context, chatID int64, query string) {
	if strings.TrimSpace(query) == "" {
		_ = SendMessage(ctx, h.api, chatID, "Использование: /search <запрос>")
		return
	}
	qvec, err := h.vec.EmbedText(ctx, query)
	if err != nil {
		_ = SendMessage(ctx, h.api, chatID, "Ошибка поиска: не удалось векторизовать запрос.")
		return
	}
	rows, err := h.vec.SearchTopK(ctx, chatID, qvec, 10)
	if err != nil {
		_ = SendMessage(ctx, h.api, chatID, "Ошибка поиска по истории.")
		return
	}
	_ = SendMessage(ctx, h.api, chatID, "🔍 По запросу «"+query+"»:\n\n"+messages.FormatSearchResult(rows))
}

func (h *Handlers) cmdTopic(ctx context.Context, chatID int64, msg *models.Message, args string) {
	if !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, chatID, "Команда только для админов.")
		return
	}
	if strings.TrimSpace(args) != "now" {
		_ = SendMessage(ctx, h.api, chatID, "Использование: /topic now")
		return
	}
	// PostNow сам постит тему через Scheduler.post() — повторно не отправляем.
	if _, err := h.topics.PostNow(ctx, chatID); err != nil {
		_ = SendMessage(ctx, h.api, chatID, "Не удалось сгенерировать тему: "+err.Error())
	}
}

func (h *Handlers) cmdDigest(ctx context.Context, chatID int64, msg *models.Message, args string) {
	if !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, chatID, "Команда только для админов.")
		return
	}
	if strings.TrimSpace(args) != "week" {
		_ = SendMessage(ctx, h.api, chatID, "Использование: /digest week")
		return
	}
	_ = SendMessage(ctx, h.api, chatID, "⏳ Собираю дайджест за неделю...")
	// Асинхронно, чтобы не блокировать update-цикл.
	go func() {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		end := time.Now()
		start := end.Add(-7 * 24 * time.Hour)
		if err := h.digester.PostDigest(ctxBg, chatID, start, end); err != nil {
			if errors.Is(err, topics.ErrTooFewMessages) {
				_ = SendMessage(ctxBg, h.api, chatID, "Недостаточно сообщений за неделю для дайджеста (нужно минимум 3).")
			} else {
				_ = SendMessage(ctxBg, h.api, chatID, "Ошибка дайджеста: "+err.Error())
			}
		}
	}()
}

func (h *Handlers) cmdCheck(ctx context.Context, chatID int64, msg *models.Message, args string) {
	if !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, chatID, "Команда только для админов.")
		return
	}
	target := strings.TrimSpace(strings.TrimPrefix(args, "@"))
	if target == "" {
		_ = SendMessage(ctx, h.api, chatID, "Использование: /check @username")
		return
	}
	// Ищем последние сообщения по username (точное совпадение без @).
	userID, lastText := h.lookupLastByUsername(ctx, target)
	if userID == 0 || lastText == "" {
		_ = SendMessage(ctx, h.api, chatID, "Не нашёл сообщений от @"+target)
		return
	}
	v := h.moderation.ManualCheck(ctx, chatID, userID, target, lastText)
	_ = SendMessage(ctx, h.api, chatID,
		fmt.Sprintf("%s @%s — вердикт: %s\nПричина: %s",
			moderation.VerdictEmoji(v.Verdict), target, v.Verdict, v.Reason))
}

func (h *Handlers) cmdKick(ctx context.Context, chatID int64, msg *models.Message, args string) {
	if !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, chatID, "Команда только для админов.")
		return
	}
	target := strings.TrimSpace(strings.TrimPrefix(args, "@"))
	// /kick принимает @username или числовой user_id.
	var userID int64
	if n, ok := parseInt64(target); ok {
		userID = n
	} else {
		userID, _ = h.lookupLastByUsername(ctx, target)
	}
	if userID == 0 {
		_ = SendMessage(ctx, h.api, chatID, "Не нашёл пользователя. /kick @username или /kick <user_id>")
		return
	}
	// Кикаем через те же primitives, что и moderation-flow (kick → unban-only-if-banned).
	if _, err := h.api.BanChatMember(ctx, &bot.BanChatMemberParams{ChatID: chatID, UserID: userID}); err != nil {
		_ = SendMessage(ctx, h.api, chatID, "Ошибка кика: "+err.Error())
		return
	}
	_, _ = h.api.UnbanChatMember(ctx, &bot.UnbanChatMemberParams{
		ChatID: chatID, UserID: userID, OnlyIfBanned: true,
	})
	_ = SendMessage(ctx, h.api, chatID, fmt.Sprintf("Пользователь %s кикнут.", target))
}

// --- вспомогательное ---

// answerChat вызывает answerer и шлёт ответ.
func (h *Handlers) answerChat(ctx context.Context, chatID int64, msg *models.Message, question string) {
	question = strings.TrimSpace(question)
	if question == "" {
		_ = SendMessage(ctx, h.api, chatID, "Спроси что-нибудь конкретнее 🙂")
		return
	}
	go func() {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		resp, err := h.answerer.Answer(ctxBg, chatID, replyUsername(msg.From), question)
		if err != nil {
			slog.Error("answer chat", "err", err)
			_ = SendMessage(ctxBg, h.api, chatID, "Не удалось получить ответ, попробуй позже.")
			return
		}
		_ = SendMessage(ctxBg, h.api, chatID, resp)
	}()
}

// saveMessage сохраняет текстовое сообщение в БД + ставит в очередь embedding.
func (h *Handlers) saveMessage(ctx context.Context, msg *models.Message, text string) {
	m := &messages.Message{
		ID:       int64(msg.ID),
		ChatID:   msg.Chat.ID,
		UserID:   msg.From.ID,
		Username: msg.From.Username,
		Text:     text,
		TS:       time.Unix(int64(msg.Date), 0),
	}
	if msg.ReplyToMessage != nil {
		rid := int64(msg.ReplyToMessage.ID)
		m.ReplyToID = &rid
	}
	if err := h.msgs.Save(ctx, m); err != nil {
		slog.Warn("save message", "err", err)
		return
	}
	h.vec.Enqueue(int64(msg.ID))
}

// isAddressedToBot — @упоминание бота (по username) или reply на сообщение бота.
func (h *Handlers) isAddressedToBot(msg *models.Message, text string) bool {
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil &&
		msg.ReplyToMessage.From.ID == h.botUserID {
		return true
	}
	// @botname — эвристика: ищем @username бота в тексте.
	if h.botUsername != "" {
		return strings.Contains(strings.ToLower(text), "@"+strings.ToLower(h.botUsername))
	}
	return false
}

// stripBotMention убирает @botname из текста вопроса.
func stripBotMention(text string, botUserID int64) string {
	// Универсальная очистка от всех @упоминаний — для вопроса это шум.
	return strings.TrimSpace(text)
}

// lookupLastByUsername ищет userID и текст последнего сообщения по username.
func (h *Handlers) lookupLastByUsername(ctx context.Context, username string) (int64, string) {
	userID, text, err := h.msgs.LastByUsername(ctx, username)
	if err != nil || userID == 0 {
		return 0, ""
	}
	return userID, text
}

// SetBotUsername — конфигуратор (вызывается из main после getMe).
func (h *Handlers) SetBotUsername(u string) { h.botUsername = u }
