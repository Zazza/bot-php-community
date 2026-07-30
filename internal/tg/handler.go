package tg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"phpbot/internal/chat"
	"phpbot/internal/faq"
	"phpbot/internal/messages"
	"phpbot/internal/moderation"
	"phpbot/internal/topics"
	"phpbot/internal/users"
)

// replyTimeout — сколько ждём LLM/дайджеста в фоне.
const replyTimeout = 90 * time.Second

// spamClassifyTimeout — budget на LLM-классификацию спама (отдельный от сохранения).
const spamClassifyTimeout = 25 * time.Second

// spamSaveTimeout — бюджет на сохранение/ответ после классификации (независим, чтобы
// сообщение не терялось, если LLM съела почти всё время классификации).
const spamSaveTimeout = 15 * time.Second

// expertMaxDist — косинусный радиус «по теме» для /expert (~0.65 сходства).
const expertMaxDist = 0.35

// Handlers — центральная диспетчеризация update. Содержит ссылки на все домены.
type Handlers struct {
	api           *bot.Bot
	chatIDs       map[int64]struct{}
	botUserID     int64  // собственный id бота, чтобы отличать reply-to-bot
	botUsername   string // ник бота для @упоминаний
	primaryChatID int64  // первый групповой чат — источник контекста для ответов в ЛС

	moderation *moderation.Flow
	spam       *moderation.SpamFilter
	vote       *moderation.VoteToKick
	users      *users.Repository
	msgs       *messages.Repository
	vec        *messages.VectorRepo
	answerer   *chat.Answerer
	topics     *topics.Scheduler
	digester   *topics.Digester
	faq        *faq.Repo
	faqBuilder *faq.Builder
}

// HandlersDeps — зависимости для сборки Handlers.
type HandlersDeps struct {
	API        *bot.Bot
	ChatIDs    []int64
	BotUserID  int64
	Moderation *moderation.Flow
	Spam       *moderation.SpamFilter
	Vote       *moderation.VoteToKick
	Users      *users.Repository
	Msgs       *messages.Repository
	Vec        *messages.VectorRepo
	Answerer   *chat.Answerer
	Topics     *topics.Scheduler
	Digester   *topics.Digester
	FAQ        *faq.Repo
	FAQBuilder *faq.Builder
}

// NewHandlers собирает Handlers.
func NewHandlers(d HandlersDeps) *Handlers {
	h := &Handlers{
		api:        d.API,
		botUserID:  d.BotUserID,
		moderation: d.Moderation,
		spam:       d.Spam,
		vote:       d.Vote,
		users:      d.Users,
		msgs:       d.Msgs,
		vec:        d.Vec,
		answerer:   d.Answerer,
		topics:     d.Topics,
		digester:   d.Digester,
		faq:        d.FAQ,
		faqBuilder: d.FAQBuilder,
		chatIDs:    make(map[int64]struct{}, len(d.ChatIDs)),
	}
	for _, id := range d.ChatIDs {
		h.chatIDs[id] = struct{}{}
	}
	if len(d.ChatIDs) > 0 {
		h.primaryChatID = d.ChatIDs[0]
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

	// 1b. left_chat_member — cleanup orphan-капчи (напр. новичка снёс Combot).
	if msg.LeftChatMember != nil {
		if _, ok := h.chatIDs[chatID]; ok {
			h.moderation.OnLeftMember(ctx, chatID, msg.LeftChatMember.ID)
		}
		return
	}

	// ЛС: админ-команды выполняем против данных группового чата (primaryChatID),
	// ответ шлём в ЛС. Свободный вопрос (ask) — через pmAnswer. Не-админам — отказ.
	if msg.Chat.Type == "private" {
		if msg.From == nil || !h.moderation.IsAdmin(msg.From.ID) {
			_ = SendMessage(ctx, h.api, msg.Chat.ID, "В личке я отвечаю только администраторам чата.")
			return
		}
		if h.primaryChatID == 0 {
			_ = SendMessage(ctx, h.api, msg.Chat.ID, "Групповой чат не настроен.")
			return
		}
		pt := msg.Text
		if pt == "" {
			pt = msg.Caption
		}
		if cmd, args := extractCommand(pt); cmd != "" {
			h.dispatchCommand(ctx, msg.Chat.ID, h.primaryChatID, msg, cmd, args)
			return
		}
		h.pmAnswer(ctx, msg)
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
		h.dispatchCommand(ctx, chatID, chatID, msg, cmd, args)
		return
	}

	// 3. Сохраняем любое текстовое сообщение в историю + async embedding.
	if text != "" && msg.From != nil {
		// 3a. Анти-спам: синхронная эвристика → при подозрении LLM-классификация в горутине.
		// Не фильтруем бота, админов и не блокируем update-цикл.
		if h.spam != nil && msg.From.ID != h.botUserID && !h.moderation.IsAdmin(msg.From.ID) {
			in := moderation.SpamInput{
				ChatID: chatID, UserID: msg.From.ID, Username: msg.From.Username,
				MessageID: int64(msg.ID), Text: text,
			}
			if hit, reason := h.spam.Heuristic(in); hit {
				go func(in moderation.SpamInput, reason string) {
					defer func() {
						if r := recover(); r != nil {
							slog.Error("spam worker panic", "err", r, "stack", string(debug.Stack()))
						}
					}()
					classCtx, classCancel := context.WithTimeout(context.Background(), spamClassifyTimeout)
					defer classCancel()
					if h.spam.ClassifyAndEnforce(classCtx, in, reason) {
						return
					}
					// Свежий бюджет на сохранение/ответ — независимо от времени LLM.
					saveCtx, saveCancel := context.WithTimeout(context.Background(), spamSaveTimeout)
					defer saveCancel()
					h.saveMessage(saveCtx, msg, in.Text)
					if h.isAddressedToBot(msg, in.Text) {
						h.answerChat(saveCtx, chatID, chatID, msg, stripBotMention(in.Text, h.botUserID))
					}
				}(in, reason)
				return
			}
		}

		// 3b. Сохраняем в историю + async embedding.
		h.saveMessage(ctx, msg, text)

		// 3c. PHP-триггеры: @упоминание бота или reply на сообщение бота.
		if h.isAddressedToBot(msg, text) {
			h.answerChat(ctx, chatID, chatID, msg, stripBotMention(text, h.botUserID))
		}
	}
}

// OnCallbackQuery — кнопки капчи новичка (gate:<id>:<opt>).
func (h *Handlers) OnCallbackQuery(ctx context.Context, b *bot.Bot, upd *models.Update) {
	cb := upd.CallbackQuery
	if cb == nil {
		return
	}
	if strings.HasPrefix(cb.Data, "gate:") {
		alert := h.moderation.HandleGateCallback(ctx, cb)
		if alert != "" {
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cb.ID, Text: alert,
			})
		}
		return
	}
	if strings.HasPrefix(cb.Data, "vote:") && h.vote != nil {
		alert := h.vote.HandleVoteCallback(ctx, cb)
		if alert == "" {
			alert = "—"
		}
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cb.ID, Text: alert,
		})
		return
	}
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID, Text: "—",
	})
}

// dispatchCommand маршрутизирует slash-команды.
// replyChatID — куда шлём ответ; dataChatID — из чата тянем данные (в ЛС = primaryChatID).
func (h *Handlers) dispatchCommand(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, cmd, args string) {
	switch cmd {
	case "ask":
		h.answerChat(ctx, replyChatID, dataChatID, msg, args)
	case "search":
		h.cmdSearch(ctx, replyChatID, dataChatID, args)
	case "expert":
		h.cmdExpert(ctx, replyChatID, dataChatID, args)
	case "help", "start":
		h.cmdHelp(ctx, replyChatID)
	case "stats":
		h.cmdStats(ctx, replyChatID, dataChatID, args)
	case "topic":
		h.cmdTopic(ctx, replyChatID, dataChatID, msg, args)
	case "digest":
		h.cmdDigest(ctx, replyChatID, dataChatID, msg, args)
	case "check":
		h.cmdCheck(ctx, replyChatID, dataChatID, msg, args)
	case "about":
		h.cmdAbout(ctx, replyChatID, dataChatID, msg, args)
	case "faq":
		h.cmdFaq(ctx, replyChatID, dataChatID, msg, args)
	case "kick":
		h.cmdKick(ctx, replyChatID, dataChatID, msg, args)
	case "report":
		h.cmdReport(ctx, replyChatID, dataChatID, msg, args)
	default:
		slog.Debug("unknown command", "cmd", cmd)
	}
}

// --- команды ---

func (h *Handlers) cmdHelp(ctx context.Context, replyChatID int64) {
	text := `*Команды бота*

/ask <вопрос> — ответ на PHP/IT-вопрос
/search <запрос> — поиск по истории чата
/expert <тема> — к кому обратиться по теме
/stats — статистика чата
/faq — список частых вопросов (или /faq <id>)
/help — этот текст

*Только для админов:*
/topic now — сгенерировать и запостить тему
/digest [период] — дайджест (week, month, 2025-06)
/check @user — ручная judge-проверка последних сообщений
/about @user [период] — краткий портрет участника по его сообщениям
/faq edit <id> <ответ> — правка ответа FAQ
/faq build — пересобрать FAQ из истории
/kick @user — кик пользователя

*Для всех:*
/report (reply на сообщение или @user) — голосование за изгнание

Бот также отвечает на @упоминание и reply на своё сообщение.`
	_ = SendMessage(ctx, h.api, replyChatID, text)
}

func (h *Handlers) cmdStats(ctx context.Context, replyChatID, dataChatID int64, args string) {
	p, err := parsePeriod(args)
	if err != nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Не понял период. Примеры: /stats, /stats week, /stats 2025, /stats 2025-06")
		return
	}
	s, err := h.msgs.Stats(ctx, dataChatID, p.since, p.until)
	if err != nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Не удалось собрать статистику.")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📊 *Статистика чата*\n\nПериод: %s\nСообщений: %d\nАктивных: %d",
		p.label, s.TotalMessages, s.ActiveUsers)
	if p.until == nil {
		fmt.Fprintf(&b, "\nЗа 24ч: %d", s.LastDay)
	}
	b.WriteString("\n\n*Топ за период:*")
	for i, pp := range s.TopPosters {
		fmt.Fprintf(&b, "\n%d. %s — %d", i+1, pp.Username, pp.Count)
	}
	_ = SendMessage(ctx, h.api, replyChatID, b.String())
}

func (h *Handlers) cmdSearch(ctx context.Context, replyChatID, dataChatID int64, args string) {
	query, username, p := parseSearchArgs(args)
	if strings.TrimSpace(query) == "" {
		_ = SendMessage(ctx, h.api, replyChatID, "Использование: /search <запрос> [@user] [период]\nПримеры: /search pgvector, /search pgvector @ivan, /search pgvector week, /search enum @ivan 2025-06")
		return
	}
	qvec, err := h.vec.EmbedText(ctx, query)
	if err != nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Ошибка поиска: не удалось векторизовать запрос.")
		return
	}
	rows, err := h.vec.SearchFiltered(ctx, dataChatID, qvec, 10, username, p.since, p.until)
	if err != nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Ошибка поиска по истории.")
		return
	}
	var b strings.Builder
	b.WriteString("🔍 По запросу «" + query + "»")
	if username != "" {
		b.WriteString(", автор @" + username)
	}
	if p.since != nil || p.until != nil {
		b.WriteString(", " + p.label)
	}
	b.WriteString("\n\n")
	b.WriteString(messages.FormatSearchResult(rows))
	_ = SendMessage(ctx, h.api, replyChatID, b.String())
}

func (h *Handlers) cmdExpert(ctx context.Context, replyChatID, dataChatID int64, args string) {
	topic := strings.TrimSpace(args)
	if topic == "" {
		_ = SendMessage(ctx, h.api, replyChatID, "Использование: /expert <тема>\nПример: /expert pgvector индексы")
		return
	}
	qvec, err := h.vec.EmbedText(ctx, topic)
	if err != nil {
		slog.Error("expert embed", "err", err, "topic", topic)
		_ = SendMessage(ctx, h.api, replyChatID, "Не удалось обработать тему.")
		return
	}
	exps, err := h.vec.Experts(ctx, dataChatID, qvec, expertMaxDist, 5)
	if err != nil {
		slog.Error("experts", "err", err, "chat", dataChatID)
		_ = SendMessage(ctx, h.api, replyChatID, "Не удалось найти экспертов.")
		return
	}
	if len(exps) == 0 {
		_ = SendMessage(ctx, h.api, replyChatID, "🎓 По теме «"+topic+"» никого конкретного в истории не нашёл.")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🎓 По теме «%s» лучше спросить:\n", topic)
	for i, e := range exps {
		fmt.Fprintf(&b, "%d. @%s — %d сообщений\n", i+1, e.Username, e.Count)
	}
	_ = SendMessage(ctx, h.api, replyChatID, b.String())
}

func (h *Handlers) cmdTopic(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, args string) {
	if !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, "Команда только для админов.")
		return
	}
	if strings.TrimSpace(args) != "now" {
		_ = SendMessage(ctx, h.api, replyChatID, "Использование: /topic now")
		return
	}
	// PostNow сам постит тему через Scheduler.post() в postChatID — повторно не отправляем.
	if _, err := h.topics.PostNow(ctx, dataChatID, replyChatID); err != nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Не удалось сгенерировать тему: "+err.Error())
	}
}

func (h *Handlers) cmdDigest(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, args string) {
	if !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, "Команда только для админов.")
		return
	}
	argStr := strings.TrimSpace(args)
	if argStr == "" {
		argStr = "week"
	}
	p, err := parsePeriod(argStr)
	if err != nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Не понял период. Примеры: /digest, /digest week, /digest month, /digest 2025-06")
		return
	}
	now := time.Now()
	end := now
	if p.until != nil {
		end = *p.until
	}
	start := now.Add(-7 * 24 * time.Hour)
	if p.since != nil {
		start = *p.since
	}
	_ = SendMessage(ctx, h.api, replyChatID, "⏳ Собираю дайджест "+p.label+"...")
	go func(replyChatID, dataChatID int64, start, end time.Time, label string) {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		if err := h.digester.PostDigest(ctxBg, dataChatID, replyChatID, start, end); err != nil {
			if errors.Is(err, topics.ErrTooFewMessages) {
				_ = SendMessage(ctxBg, h.api, replyChatID, "Недостаточно сообщений за период «"+label+"» (нужно минимум 3).")
			} else {
				_ = SendMessage(ctxBg, h.api, replyChatID, "Ошибка дайджеста: "+err.Error())
			}
		}
	}(replyChatID, dataChatID, start, end, p.label)
}

func (h *Handlers) cmdCheck(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, args string) {
	if !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, "Команда только для админов.")
		return
	}
	target := strings.TrimSpace(strings.TrimPrefix(args, "@"))
	if target == "" {
		_ = SendMessage(ctx, h.api, replyChatID, "Использование: /check @username")
		return
	}
	// Ищем последние сообщения по username (точное совпадение без @).
	userID, lastText := h.lookupLastByUsername(ctx, target)
	if userID == 0 || lastText == "" {
		_ = SendMessage(ctx, h.api, replyChatID, "Не нашёл сообщений от @"+target)
		return
	}
	v := h.moderation.ManualCheck(ctx, dataChatID, userID, target, lastText)
	_ = SendMessage(ctx, h.api, replyChatID,
		fmt.Sprintf("%s @%s — вердикт: %s\nПричина: %s",
			moderation.VerdictEmoji(v.Verdict), target, v.Verdict, v.Reason))
}

func (h *Handlers) cmdAbout(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, args string) {
	if !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, "Команда только для админов.")
		return
	}
	// Первое @-поле — username, остальное — строка периода.
	fields := strings.Fields(args)
	var username string
	var rest []string
	for _, tok := range fields {
		if strings.HasPrefix(tok, "@") && username == "" {
			username = strings.TrimPrefix(tok, "@")
			continue
		}
		rest = append(rest, tok)
	}
	if username == "" {
		_ = SendMessage(ctx, h.api, replyChatID, "Использование: /about @username [период]\nПримеры: /about @ivan, /about @ivan 2025-06, /about @ivan week")
		return
	}
	p, err := parsePeriod(strings.Join(rest, " "))
	if err != nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Не понял период. Примеры: (без), week, month, 2025, 2025-06")
		return
	}
	_ = SendMessage(ctx, h.api, replyChatID, "⏳ Собираю профиль @"+username+" "+p.label+"...")
	// Асинхронно, чтобы не блокировать update-цикл. Контекст — из dataChatID (группы),
	// ответ — в replyChatID (допускает ЛС админа).
	go func(replyChatID, dataChatID int64, username string, since, until *time.Time, label string) {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		resp, err := h.answerer.About(ctxBg, dataChatID, username, since, until, label)
		if err != nil {
			slog.Error("about", "err", err, "user", username)
			_ = SendMessage(ctxBg, h.api, replyChatID, "Не удалось собрать профиль.")
			return
		}
		_ = SendMessage(ctxBg, h.api, replyChatID, "🧑 @"+username+" — "+label+"\n\n"+resp)
	}(replyChatID, dataChatID, username, p.since, p.until, p.label)
}

const faqListMaxRunes = 4000 // TG лимит 4096, оставляем запас на заголовок

func (h *Handlers) cmdFaq(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, args string) {
	if h.faq == nil {
		_ = SendMessage(ctx, h.api, replyChatID, "FAQ не настроен.")
		return
	}
	fields := strings.Fields(args)
	sub := ""
	if len(fields) > 0 {
		sub = fields[0]
	}

	switch sub {
	case "", "list":
		h.faqList(ctx, replyChatID, dataChatID)
	case "edit":
		h.faqEdit(ctx, replyChatID, msg, fields, args)
	case "build":
		h.faqBuild(ctx, replyChatID, dataChatID, msg)
	default:
		if id, ok := parseInt64(sub); ok {
			h.faqShow(ctx, replyChatID, id)
		} else {
			_ = SendMessage(ctx, h.api, replyChatID, faqUsage)
		}
	}
}

func (h *Handlers) faqList(ctx context.Context, replyChatID, dataChatID int64) {
	items, err := h.faq.List(ctx, dataChatID)
	if err != nil {
		slog.Error("faq list", "err", err, "chat", dataChatID)
		_ = SendMessage(ctx, h.api, replyChatID, "Не удалось загрузить FAQ.")
		return
	}
	if len(items) == 0 {
		_ = SendMessage(ctx, h.api, replyChatID, "FAQ пока пуст (/faq build соберёт).")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📚 FAQ (%d):\n", len(items))
	for _, it := range items {
		q := it.Question
		if r := []rune(q); len(r) > 60 {
			q = string(r[:60]) + "…"
		}
		line := fmt.Sprintf("\n%d. %s", it.ID, q)
		if b.Len()+len(line) > faqListMaxRunes {
			b.WriteString("\n…(обрезано)")
			break
		}
		b.WriteString(line)
	}
	_ = SendMessage(ctx, h.api, replyChatID, b.String())
}

func (h *Handlers) faqShow(ctx context.Context, replyChatID int64, id int64) {
	it, err := h.faq.Get(ctx, id)
	if err != nil {
		slog.Error("faq get", "err", err, "id", id)
		_ = SendMessage(ctx, h.api, replyChatID, "Не удалось загрузить запись.")
		return
	}
	if it == nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Запись не найдена.")
		return
	}
	_ = SendMessage(ctx, h.api, replyChatID, fmt.Sprintf("❓ %s\n\n💡 %s", it.Question, it.Answer))
}

func (h *Handlers) faqEdit(ctx context.Context, replyChatID int64, msg *models.Message, fields []string, args string) {
	if msg.From == nil || !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, "Команда только для админов.")
		return
	}
	if len(fields) < 2 {
		_ = SendMessage(ctx, h.api, replyChatID, "Использование: /faq edit <id> <ответ>")
		return
	}
	id, ok := parseInt64(fields[1])
	if !ok {
		_ = SendMessage(ctx, h.api, replyChatID, "id должен быть числом.")
		return
	}
	answer := strings.TrimSpace(strings.TrimPrefix(args, "edit"))
	answer = strings.TrimSpace(strings.TrimPrefix(answer, fields[1]))
	if answer == "" {
		_ = SendMessage(ctx, h.api, replyChatID, "Текст ответа пуст.")
		return
	}
	if err := h.faq.UpdateAnswer(ctx, id, answer); err != nil {
		slog.Error("faq update", "err", err, "id", id)
		_ = SendMessage(ctx, h.api, replyChatID, "Не удалось обновить ответ.")
		return
	}
	_ = SendMessage(ctx, h.api, replyChatID, fmt.Sprintf("✅ Ответ #%d обновлён.", id))
}

func (h *Handlers) faqBuild(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message) {
	if msg.From == nil || !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, "Команда только для админов.")
		return
	}
	if h.faqBuilder == nil {
		_ = SendMessage(ctx, h.api, replyChatID, "FAQ-билдер не настроен.")
		return
	}
	_ = SendMessage(ctx, h.api, replyChatID, "⏳ Собираю FAQ из истории...")
	go func(replyChatID, dataChatID int64) {
		ctxBg, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		n, err := h.faqBuilder.Build(ctxBg, dataChatID)
		if err != nil {
			slog.Error("faq build", "err", err, "chat", dataChatID)
			_ = SendMessage(ctxBg, h.api, replyChatID, "Ошибка сборки FAQ: "+err.Error())
			return
		}
		_ = SendMessage(ctxBg, h.api, replyChatID, fmt.Sprintf("FAQ собран: %d записей.", n))
	}(replyChatID, dataChatID)
}

const faqUsage = "Использование:\n/faq — список\n/faq <id> — показать\n/faq edit <id> <ответ> — правка (админ)\n/faq build — пересобрать (админ)"

func (h *Handlers) cmdKick(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, args string) {
	if !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, "Команда только для админов.")
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
		_ = SendMessage(ctx, h.api, replyChatID, "Не нашёл пользователя. /kick @username или /kick <user_id>")
		return
	}
	// Обратимый кик в групповом чате (dataChatID), отчёт — в replyChatID.
	if err := h.moderation.KickReversible(ctx, dataChatID, userID); err != nil {
		slog.Error("kick", "err", err, "user", userID)
		_ = SendMessage(ctx, h.api, replyChatID, "Не удалось кикнуть пользователя, попробуй позже.")
		return
	}
	_ = SendMessage(ctx, h.api, replyChatID, fmt.Sprintf("Пользователь %s кикнут.", target))
}

// cmdReport запускает голосование за изгнание участника.
// Таргет: reply на сообщение нарушителя ИЛИ @username/числовой id в args.
// В ЛС голосование не запускается — оно привязано к чату (кнопки/кворум).
func (h *Handlers) cmdReport(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, args string) {
	if h.vote == nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Голосование выключено.")
		return
	}
	if replyChatID != dataChatID {
		_ = SendMessage(ctx, h.api, replyChatID, "Голосование запускается только в чате — ответь /report в PHP_VRN")
		return
	}
	if msg.From == nil {
		return
	}
	fromID := msg.From.ID

	var targetID int64
	var targetUsername string
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		targetID = msg.ReplyToMessage.From.ID
		targetUsername = msg.ReplyToMessage.From.Username
	} else {
		fields := strings.Fields(args)
		if len(fields) > 0 {
			t := strings.TrimPrefix(fields[0], "@")
			if n, ok := parseInt64(t); ok {
				targetID = n
			} else {
				targetID, _ = h.lookupLastByUsername(ctx, t)
				targetUsername = t
			}
		}
	}
	if targetID == 0 {
		_ = SendMessage(ctx, h.api, replyChatID, "Сделай reply на сообщение нарушителя или укажи @username: /report @user")
		return
	}

	reason := ""
	if msg.ReplyToMessage != nil {
		reason = strings.TrimSpace(args)
	} else {
		fs := strings.Fields(args)
		if len(fs) > 1 {
			reason = strings.TrimSpace(strings.Join(fs[1:], " "))
		}
	}
	if reason == "" {
		reason = "жалоба участников чата"
	}

	switch {
	case targetID == fromID:
		_ = SendMessage(ctx, h.api, replyChatID, "Нельзя репортить себя 🙂")
		return
	case targetID == h.botUserID:
		_ = SendMessage(ctx, h.api, replyChatID, "Бота изгнать нельзя 🙂")
		return
	case h.moderation.IsAdmin(targetID):
		_ = SendMessage(ctx, h.api, replyChatID, "Админов изгонять нельзя.")
		return
	}
	if targetUsername == "" {
		targetUsername = fmt.Sprintf("user_%d", targetID)
	}

	if err := h.vote.StartVote(ctx, dataChatID, targetID, targetUsername, reason, fromID); err != nil {
		switch {
		case errors.Is(err, moderation.ErrVoteAlreadyActive), errors.Is(err, moderation.ErrReportCooldown):
			_ = SendMessage(ctx, h.api, replyChatID, err.Error())
		default:
			slog.Error("start vote", "err", err, "reporter", fromID)
			_ = SendMessage(ctx, h.api, replyChatID, "Не удалось запустить голосование, попробуй позже.")
		}
	}
}

// --- вспомогательное ---

// answerChat вызывает answerer (контекст из dataChatID) и шлёт ответ в replyChatID.
func (h *Handlers) answerChat(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, question string) {
	question = strings.TrimSpace(question)
	if question == "" {
		_ = SendMessage(ctx, h.api, replyChatID, "Спроси что-нибудь конкретнее 🙂")
		return
	}
	go func() {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		resp, err := h.answerer.Answer(ctxBg, dataChatID, replyUsername(msg.From), question)
		if err != nil {
			slog.Error("answer chat", "err", err)
			_ = SendMessage(ctxBg, h.api, replyChatID, "Не удалось получить ответ, попробуй позже.")
			return
		}
		_ = SendMessage(ctxBg, h.api, replyChatID, resp)
	}()
}

// pmAnswer — ответ в личке. Доступ только админам. Контекст (RAG + свежие) — из
// группового чата (primaryChatID), ответ шлётся в ЛС. Сообщения ЛС в историю не пишем.
func (h *Handlers) pmAnswer(ctx context.Context, msg *models.Message) {
	if msg.From == nil || !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, msg.Chat.ID, "В личке я отвечаю только администраторам чата.")
		return
	}
	if h.primaryChatID == 0 {
		_ = SendMessage(ctx, h.api, msg.Chat.ID, "Групповой чат не настроен.")
		return
	}
	q := strings.TrimSpace(msg.Text)
	if q == "" && msg.Caption != "" {
		q = msg.Caption
	}
	if cmd, args := extractCommand(q); cmd == "ask" { // опционально "/ask вопрос"
		q = strings.TrimSpace(args)
	}
	if q == "" {
		_ = SendMessage(ctx, h.api, msg.Chat.ID, "Спроси что-нибудь конкретнее 🙂")
		return
	}
	asker := replyUsername(msg.From)
	go func() {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		resp, err := h.answerer.Answer(ctxBg, h.primaryChatID, asker, q)
		if err != nil {
			slog.Error("pm answer", "err", err)
			_ = SendMessage(ctxBg, h.api, msg.Chat.ID, "Не удалось получить ответ, попробуй позже.")
			return
		}
		_ = SendMessage(ctxBg, h.api, msg.Chat.ID, resp)
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
