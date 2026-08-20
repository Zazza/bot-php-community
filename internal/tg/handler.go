package tg

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"phpbot/internal/announce"
	"phpbot/internal/chat"
	"phpbot/internal/faq"
	"phpbot/internal/messages"
	"phpbot/internal/moderation"
	"phpbot/internal/news"
	"phpbot/internal/quiz"
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

// searchMinLen — минимальная длина (в символах) сообщения для /search. Отсекает
// bare-токены («yii3», «yii2») на уровне SQL, чтобы они не забивали top-K по косинусу
// и не вытесняли содержательные сообщения из пула ранжирования.
const searchMinLen = 30

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
	spamEsc    *moderation.SpamEscalation
	users      *users.Repository
	msgs       *messages.Repository
	vec        *messages.VectorRepo
	answerer   *chat.Answerer
	digester   *topics.Digester
	faq        *faq.Repo
	faqBuilder *faq.Builder
	quiz       *quiz.Quiz
	news       *news.Digester
	announce   *announce.Service
}

// HandlersDeps — зависимости для сборки Handlers.
type HandlersDeps struct {
	API        *bot.Bot
	ChatIDs    []int64
	BotUserID  int64
	Moderation *moderation.Flow
	Spam       *moderation.SpamFilter
	Vote       *moderation.VoteToKick
	SpamEsc    *moderation.SpamEscalation
	Users      *users.Repository
	Msgs       *messages.Repository
	Vec        *messages.VectorRepo
	Answerer   *chat.Answerer
	Digester   *topics.Digester
	FAQ        *faq.Repo
	FAQBuilder *faq.Builder
	Quiz       *quiz.Quiz
	News       *news.Digester
	Announce   *announce.Service
}

// NewHandlers собирает Handlers.
func NewHandlers(d HandlersDeps) *Handlers {
	h := &Handlers{
		api:        d.API,
		botUserID:  d.BotUserID,
		moderation: d.Moderation,
		spam:       d.Spam,
		vote:       d.Vote,
		spamEsc:    d.SpamEsc,
		users:      d.Users,
		msgs:       d.Msgs,
		vec:        d.Vec,
		answerer:   d.Answerer,
		digester:   d.Digester,
		faq:        d.FAQ,
		faqBuilder: d.FAQBuilder,
		quiz:       d.Quiz,
		news:       d.News,
		announce:   d.Announce,
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
			h.moderation.OnNewMember(ctx, chatID, u.ID, u.Username, int64(msg.ID))
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

	// Запомнить @handle автора (все сообщения, включая ЛС) + ленивый бэкфилл
	// истории: импортированные сообщения хранили display-name, не @handle.
	if msg.From != nil && msg.From.ID != h.botUserID && msg.From.Username != "" {
		if err := h.users.TouchUser(ctx, msg.From.ID, msg.From.Username); err != nil {
			slog.Warn("touch user", "err", err)
		}
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
		// Режим ввода анонса (/announce): следующее сообщение становится текстом анонса.
		if h.announce != nil && h.announce.ConsumeText(ctx, msg.From.ID, pt, msg.Chat.ID) {
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
		// 3a. Анти-спам. Не фильтруем бота и админов; не блокируем update-цикл (LLM — в горутине).
		if h.spam != nil && msg.From.ID != h.botUserID && !h.moderation.IsAdmin(msg.From.ID) {
			in := moderation.SpamInput{
				ChatID: chatID, UserID: msg.From.ID, Username: msg.From.Username,
				Name:      moderation.FullName(msg.From.FirstName, msg.From.LastName),
				MessageID: int64(msg.ID), Text: text,
			}
			// Жёсткая эвристика → классификация/удаление (hard-сигналы — без LLM).
			if hit, reason := h.spam.Heuristic(in); hit {
				h.classifyInBackground(in, reason, msg, chatID)
				return
			}
			// Малоактивный/новый автор → полная LLM-классификация даже без сигнала эвристики
			// (ловит семантический текст-скам без ссылок/@/CAPS, который иначе проскакивает).
			if h.spam.IsAtRisk(ctx, in) {
				h.classifyInBackground(in, "review", msg, chatID)
				return
			}
		}

		// 3b. Сохраняем в историю + async embedding.
		h.saveMessage(ctx, msg, text)

		// 3c. PHP-триггеры: @упоминание бота или reply на сообщение бота.
		if h.isAddressedToBot(msg, text) {
			h.answerChat(ctx, chatID, chatID, msg, stripBotMention(text, h.botUserID), false)
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
	if strings.HasPrefix(cb.Data, "spamesc:") && h.spamEsc != nil {
		alert := h.spamEsc.HandleCallback(ctx, cb)
		if alert == "" {
			alert = "—"
		}
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cb.ID, Text: alert,
		})
		return
	}
	if strings.HasPrefix(cb.Data, "quiz:") && h.quiz != nil {
		text, showAlert := h.quiz.HandleQuizCallback(ctx, cb)
		if text != "" {
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: cb.ID, Text: text, ShowAlert: showAlert,
			})
		}
		return
	}
	if strings.HasPrefix(cb.Data, "announce:") && h.announce != nil {
		alert := h.announce.HandleCallback(ctx, cb)
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
		h.answerChat(ctx, replyChatID, dataChatID, msg, args, true)
	case "search":
		h.cmdSearch(ctx, replyChatID, dataChatID, args)
	case "expert":
		h.cmdExpert(ctx, replyChatID, dataChatID, args)
	case "help", "start":
		h.cmdHelp(ctx, replyChatID)
	case "stats":
		h.cmdStats(ctx, replyChatID, dataChatID, args)
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
	case "я":
		h.cmdMe(ctx, replyChatID, dataChatID, msg)
	case "quiz":
		h.cmdQuiz(ctx, replyChatID, dataChatID, msg)
	case "news":
		h.cmdNews(ctx, replyChatID, dataChatID, msg)
	case "announce":
		h.cmdAnnounce(ctx, replyChatID, msg)
	case "cancel":
		h.cmdCancelAnnounce(ctx, replyChatID, msg)
	default:
		slog.Debug("unknown command", "cmd", cmd)
	}
}

// --- команды ---

func (h *Handlers) cmdHelp(ctx context.Context, replyChatID int64) {
	text := `*Команды бота*

/ask <вопрос> — ответ на PHP/IT-вопрос (история чата + свежее из веба)
/search <запрос> — поиск по истории чата
/expert <тема> — к кому обратиться по теме
/stats — статистика чата
/я — твоя карточка по истории чата
/faq — список частых вопросов (или /faq <id>)
/help — этот текст

*Только для админов:*
/digest [период] — дайджест (week, month, 2025-06)
/check @user — ручная judge-проверка последних сообщений
/about @user [период] — краткий портрет участника по его сообщениям
/faq edit <id> <ответ> — правка ответа FAQ
/faq build — пересобрать FAQ из истории
/kick @user — кик пользователя
/quiz — запостить вопрос викторины
/news — PHP-новости недели (релизы/пакеты/обсуждения)
/announce (в личке боту) — анонс митапа со сбором спикеров

*Для всех:*
/report (reply на сообщение или @user) — голосование за изгнание

Бот также отвечает на @упоминание и reply на своё сообщение — но только по истории чата (если тема обсуждалась).`
	_ = SendMessage(ctx, h.api, replyChatID, text)
}

// cmdMe — личная карточка участника по его истории (стаж, активность, стиль — через LLM).
// Публично в чат. dataChatID — по какому чату считаем статистику.
func (h *Handlers) cmdMe(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message) {
	if msg.From == nil {
		return
	}
	userID := msg.From.ID
	go func() {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		st, err := h.msgs.UserStats(ctxBg, dataChatID, userID)
		if err != nil {
			slog.Error("me user stats", "err", err)
			_ = SendMessage(ctxBg, h.api, replyChatID, "Не удалось собрать статистику.")
			return
		}
		oldest, _ := h.msgs.FirstByUser(ctxBg, userID, 20)
		recent, _ := h.msgs.LastByUser(ctxBg, userID, 20)

		var b strings.Builder
		fmt.Fprintf(&b, "Участник: %s\n", replyUsername(msg.From))
		if !st.FirstTS.IsZero() {
			years := time.Now().Year() - st.FirstTS.Year()
			fmt.Fprintf(&b, "В чате с: %s (%d %s)\n", st.FirstTS.Format("02.01.2006"), years, pluralYears(years))
		}
		fmt.Fprintf(&b, "Сообщений: %d\n", st.Count)
		fmt.Fprintf(&b, "Средняя длина: %.0f симв.\n", st.AvgLen)
		fmt.Fprintf(&b, "Сообщений с кодом: %d\n", st.CodeMsgs)
		if st.PeakHour >= 0 {
			fmt.Fprintf(&b, "Пик активности: %02d:00–%02d:00\n", st.PeakHour, (st.PeakHour+1)%24)
		}
		b.WriteString("\n[Ранние сообщения — как начинал]\n")
		for _, m := range oldest {
			line := strings.ReplaceAll(m.Text, "\n", " ")
			if len(line) > 160 {
				line = line[:160] + "…"
			}
			fmt.Fprintf(&b, "[%s] %s\n", m.TS.Format("02.01.2006"), line)
		}
		b.WriteString("\n[Недавние сообщения]\n")
		for _, m := range recent {
			line := strings.ReplaceAll(m.Text, "\n", " ")
			if len(line) > 160 {
				line = line[:160] + "…"
			}
			fmt.Fprintf(&b, "[%s] %s\n", m.TS.Format("02.01.2006"), line)
		}
		resp, err := h.answerer.Profile(ctxBg, b.String())
		if err != nil {
			slog.Error("me profile", "err", err)
			_ = SendMessage(ctxBg, h.api, replyChatID, "Не удалось собрать карточку, попробуй позже.")
			return
		}
		_ = SendMessage(ctxBg, h.api, replyChatID, resp)
	}()
}

// cmdQuiz — админский запуск викторины: вопрос постится в групповой чат (dataChatID).
func (h *Handlers) cmdQuiz(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message) {
	if msg.From == nil || !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, "Команда только для админов.")
		return
	}
	if h.quiz == nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Викторина выключена.")
		return
	}
	go func() {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		if err := h.quiz.Post(ctxBg, dataChatID); err != nil {
			slog.Error("quiz post", "err", err)
			_ = SendMessage(ctxBg, h.api, replyChatID, "Не удалось собрать вопрос викторины: "+err.Error())
		}
	}()
}

// cmdNews — админский запуск PHP-дайджеста. Постит туда, где вызван: в группе → в группу,
// в ЛС → приватный превью админу (дедуп news_posted привязан к chat_id, поэтому превью в ЛС
// не «съедает» недельные новости группы). dataChatID не используется (новости — внешний контент).
// args="fake" — ручной запуск пятничного выпуска (без fallback на обычный дайджест).
func (h *Handlers) cmdNews(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message) {
	if msg.From == nil || !h.moderation.IsAdmin(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, "Команда только для админов.")
		return
	}
	if h.news == nil {
		_ = SendMessage(ctx, h.api, replyChatID, "PHP-новости выключены.")
		return
	}
	_, args := extractCommand(msg.Text)
	// Первый токен регистронезависимо: "/news FAKE", "/news Fake погнали" — тоже рубрика.
	if fs := strings.Fields(args); len(fs) > 0 && strings.EqualFold(fs[0], "fake") {
		_ = SendMessage(ctx, h.api, replyChatID, "🎰 Собираю пятничный выпуск...")
		go func() {
			// 2 минуты: LLM HTTP-таймаут 90s + запас на постинг (replyTimeout впритык).
			ctxBg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := h.news.PostFake(ctxBg, replyChatID); err != nil {
				slog.Error("fake news post", "err", err)
				_ = SendMessage(ctxBg, h.api, replyChatID, "Не удалось собрать пятничный выпуск: "+err.Error())
			}
		}()
		return
	}
	_ = SendMessage(ctx, h.api, replyChatID, "⏳ Собираю PHP-новости недели...")
	go func() {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		if err := h.news.Post(ctxBg, replyChatID); err != nil {
			if errors.Is(err, news.ErrNoNews) {
				_ = SendMessage(ctxBg, h.api, replyChatID, "Нет свежих PHP-новостей на этот раз.")
			} else {
				slog.Error("news post", "err", err)
				_ = SendMessage(ctxBg, h.api, replyChatID, "Не удалось собрать новости: "+err.Error())
			}
		}
	}()
}

// cmdAnnounce — запуск ввода анонса митапа. Доступен только из ЛС админа; в группе и от
// не-админов молча игнорируется. Сам текст анонса принимает ConsumeText (следующее сообщение).
func (h *Handlers) cmdAnnounce(ctx context.Context, replyChatID int64, msg *models.Message) {
	if msg.Chat.Type != "private" || msg.From == nil || !h.moderation.IsAdmin(msg.From.ID) {
		return
	}
	if h.announce == nil {
		return
	}
	h.announce.Start(ctx, replyChatID, msg.From.ID)
}

// cmdCancelAnnounce — сброс режима ввода анонса (/cancel в личке).
func (h *Handlers) cmdCancelAnnounce(ctx context.Context, replyChatID int64, msg *models.Message) {
	if msg.Chat.Type != "private" || msg.From == nil || !h.moderation.IsAdmin(msg.From.ID) {
		return
	}
	if h.announce == nil {
		return
	}
	if h.announce.Cancel(msg.From.ID) {
		_ = SendMessage(ctx, h.api, replyChatID, announce.CancelText)
	}
}

// pluralYears — склонение «год/года/лет».
func pluralYears(n int) string {
	if n%100 >= 11 && n%100 <= 14 {
		return "лет"
	}
	switch n % 10 {
	case 1:
		return "год"
	case 2, 3, 4:
		return "года"
	default:
		return "лет"
	}
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
	rows, err := h.vec.SearchFiltered(ctx, dataChatID, qvec, 12, username, p.since, p.until, searchMinLen)
	if err != nil {
		_ = SendMessage(ctx, h.api, replyChatID, "Ошибка поиска по истории.")
		return
	}
	msgs := substantiveMessages(rows)
	if len(msgs) == 0 {
		_ = SendMessage(ctx, h.api, replyChatID, "🔍 По теме «"+query+"» ничего содержательного в истории не нашёл.")
		return
	}
	_ = SendMessage(ctx, h.api, replyChatID, "⏳ Собираю обзор по теме «"+query+"»...")
	go func(replyChatID int64, query, username, label string, hasFilter bool, msgs []messages.Message) {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		header := "🔍 Обзор по теме «" + query + "»"
		if username != "" {
			header += ", автор @" + username
		}
		if hasFilter {
			header += ", " + label
		}
		header += ":\n\n"
		overview, err := h.answerer.TopicOverview(ctxBg, query, msgs)
		if err != nil {
			slog.Warn("search overview", "err", err, "topic", query)
			_ = SendMessage(ctxBg, h.api, replyChatID, header+numberedCitations(msgs))
			return
		}
		_ = SendMessage(ctxBg, h.api, replyChatID, header+overview+"\n\n📖 Источники:\n"+numberedCitations(msgs))
	}(replyChatID, query, username, p.label, p.since != nil || p.until != nil, msgs)
}

// substantiveMessages дедуплицирует результаты поиска (lowercased+trimmed) и обрезает
// до 10. Фильтр длины выполняется в SQL (search, minLen) — отсекает bare-токены ДО
// ранжирования. rows уже отсортированы по убыванию сходства, поэтому первые N уникальных
// — наиболее релевантные. Порядок сохраняется — он задаёт нумерацию [N] для обзора.
func substantiveMessages(rows []messages.SearchMessage) []messages.Message {
	out := make([]messages.Message, 0, 10)
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if len(out) >= 10 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(r.Text))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r.Message)
	}
	return out
}

// numberedCitations формирует список источников: «N. [дд.мм чч:мм] автор: текст».
// Текст обрезается до 120 рун. Нумерация 1..N совпадает с нумерацией [N] в контексте
// обзорного промпта (тот же порядок msgs).
func numberedCitations(msgs []messages.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		who := m.Username
		if who == "" {
			who = "user"
		}
		text := m.Text
		if r := []rune(text); len(r) > 120 {
			text = string(r[:120]) + "…"
		}
		fmt.Fprintf(&b, "%d. [%s] %s: %s\n", i+1, m.TS.Format("02.01 15:04"), who, text)
	}
	return b.String()
}

func (h *Handlers) cmdExpert(ctx context.Context, replyChatID, dataChatID int64, args string) {
	topic := strings.TrimSpace(args)
	if topic == "" {
		_ = SendMessage(ctx, h.api, replyChatID, "Использование: /expert <тема>\nПример: /expert pgvector индексы")
		return
	}
	exps, err := h.msgs.ExpertByKeyword(ctx, dataChatID, topic, 5)
	if err != nil {
		slog.Error("experts", "err", err, "chat", dataChatID)
		_ = SendMessage(ctx, h.api, replyChatID, "Не удалось найти экспертов.")
		return
	}
	if len(exps) == 0 {
		_ = SendMessage(ctx, h.api, replyChatID, "🎓 По теме «"+topic+"» никто не упоминал в истории. Попробуй синоним.")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🎓 По теме «%s» лучше спросить:\n", html.EscapeString(topic))
	for i, e := range exps {
		name := e.Username
		// В БД только display-name (юзера не видели живьём) — достаём актуальный @handle
		// у Telegram и кэшируем. Если хэндла нет вовсе — останется имя без «@».
		if !isTelegramHandle(name) {
			if live := h.resolveExpertHandle(ctx, dataChatID, e.UserID); isTelegramHandle(live) {
				name = live
			}
		}
		b.WriteString(expertLine(i+1, e.UserID, name, e.Count))
	}
	if _, err := h.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: replyChatID, Text: b.String(), ParseMode: models.ParseModeHTML,
	}); err != nil {
		slog.Warn("expert send html", "err", err, "chat", replyChatID)
	}
}

// expertLine формирует строку списка /expert: настоящий @handle — с «@»,
// иначе имя/отображаемое имя — без «@» (кликабельно через tg://user?id= ссылку).
// Пустое имя → «user». i — 1-based номер.
func expertLine(i int, userID int64, username string, count int) string {
	name := strings.TrimSpace(username)
	visible := name
	if isTelegramHandle(name) {
		visible = "@" + name
	} else if visible == "" {
		visible = "user"
	}
	return fmt.Sprintf("%d. <a href=\"tg://user?id=%d\">%s</a> — %d сообщений\n",
		i, userID, html.EscapeString(visible), count)
}

// isTelegramHandle — похоже ли значение на настоящий TG @username: 5–32 символа из
// [A-Za-z0-9_], без пробелов и не-ASCII. Отличает handle от display-name.
func isTelegramHandle(s string) bool {
	if len(s) < 5 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}

// resolveExpertHandle достаёт актуальный @handle участника через Telegram API (getChatMember)
// и кэширует его (TouchUser → lazy backfill истории, следующий раз без запроса).
// Non-fatal: ошибка или отсутствие хэндла → "". Только для тех, у кого в БД display-name.
func (h *Handlers) resolveExpertHandle(ctx context.Context, chatID, userID int64) string {
	cm, err := h.api.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chatID, UserID: userID})
	if err != nil {
		slog.Warn("expert getChatMember", "err", err, "user_id", userID)
		return ""
	}
	handle := memberUsername(cm)
	if handle != "" {
		if err := h.users.TouchUser(ctx, userID, handle); err != nil {
			slog.Warn("expert touch user", "err", err, "user_id", userID)
		}
	}
	return handle
}

// memberUsername достаёт @handle из ChatMember любого статуса. Пусто, если нет.
func memberUsername(cm *models.ChatMember) string {
	if cm == nil {
		return ""
	}
	var u *models.User
	switch cm.Type {
	case models.ChatMemberTypeOwner:
		if cm.Owner != nil {
			u = cm.Owner.User
		}
	case models.ChatMemberTypeAdministrator:
		if cm.Administrator != nil {
			u = &cm.Administrator.User
		}
	case models.ChatMemberTypeMember:
		if cm.Member != nil {
			u = cm.Member.User
		}
	case models.ChatMemberTypeRestricted:
		if cm.Restricted != nil {
			u = cm.Restricted.User
		}
	case models.ChatMemberTypeLeft:
		if cm.Left != nil {
			u = cm.Left.User
		}
	case models.ChatMemberTypeBanned:
		if cm.Banned != nil {
			u = cm.Banned.User
		}
	}
	if u != nil {
		return u.Username
	}
	return ""
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
		// Неделя = два последовательных LLM-вызова (основа + ретро) по 90с worst case
		// каждый: replyTimeout (90с) их не покрывает, берём собственный бюджет (как cron).
		ctxBg, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
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
		if it.Answer != "" {
			a := it.Answer
			if r := []rune(a); len(r) > 80 {
				a = string(r[:80]) + "…"
			}
			line += fmt.Sprintf("\n   ↳ %s", a)
		}
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
	var targetName string
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil {
		targetID = msg.ReplyToMessage.From.ID
		targetUsername = msg.ReplyToMessage.From.Username
		targetName = moderation.FullName(msg.ReplyToMessage.From.FirstName, msg.ReplyToMessage.From.LastName)
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

	if err := h.vote.StartVote(ctx, dataChatID, targetID, targetUsername, targetName, reason, fromID); err != nil {
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

// replyToBot — триггер: ответ reply на сообщение самого бота.
func replyToBot(msg *models.Message, botUserID int64) bool {
	return msg != nil && msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil &&
		msg.ReplyToMessage.From.ID == botUserID
}

// enrichQuestion добавляет контекст ответа-на-бота: если триггер — reply на сообщение
// самого бота, препендим его текст (обрезка), чтобы RAG якорился на процитированном факте
// (кейс «тегают на то, что он сказал»), а не на пустой текст reply.
func enrichQuestion(question string, msg *models.Message, botUserID int64) string {
	if !replyToBot(msg, botUserID) {
		return question
	}
	ref := strings.TrimSpace(msg.ReplyToMessage.Text)
	if ref == "" {
		return question
	}
	if len(ref) > 1500 {
		ref = ref[:1500] + "…"
	}
	if question == "" {
		return "Сообщение, на которое ответили:\n" + ref
	}
	return "Сообщение, на которое ответили:\n" + ref + "\n\nВопрос: " + question
}

// answerChat вызывает answerer (контекст из dataChatID) и шлёт ответ в replyChatID.
// explicit=true (/ask) — AnswerAsk: ответ гарантирован (история+веб+знания);
// false (упоминание/reply) — Answer: история-only, пустой ответ = молчание.
func (h *Handlers) answerChat(ctx context.Context, replyChatID, dataChatID int64, msg *models.Message, question string, explicit bool) {
	question = enrichQuestion(strings.TrimSpace(question), msg, h.botUserID)
	if question == "" {
		_ = SendMessage(ctx, h.api, replyChatID, "Спроси что-нибудь конкретнее 🙂")
		return
	}
	go func() {
		ctxBg, cancel := context.WithTimeout(context.Background(), replyTimeout)
		defer cancel()
		asker := replyUsername(msg.From)
		var resp string
		var err error
		if explicit {
			resp, err = h.answerer.AnswerAsk(ctxBg, dataChatID, asker, question)
		} else {
			resp, err = h.answerer.Answer(ctxBg, dataChatID, asker, question)
		}
		if err != nil {
			slog.Error("answer chat", "err", err)
			_ = SendMessage(ctxBg, h.api, replyChatID, "Не удалось получить ответ, попробуй позже.")
			return
		}
		if resp != "" { // пустой ответ (SKIP/не нашёл) — промолчим, «не обсуждали» не выводим
			_ = SendMessage(ctxBg, h.api, replyChatID, resp)
		}
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
		resp, err := h.answerer.AnswerAsk(ctxBg, h.primaryChatID, asker, q)
		if err != nil {
			slog.Error("pm answer", "err", err)
			_ = SendMessage(ctxBg, h.api, msg.Chat.ID, "Не удалось получить ответ, попробуй позже.")
			return
		}
		if resp != "" { // пустой ответ (SKIP/не нашёл) — промолчим
			_ = SendMessage(ctxBg, h.api, msg.Chat.ID, resp)
		}
	}()
}

// classifyInBackground запускает LLM-классификацию/удаление спама в горутине, чтобы не
// блокировать update-цикл. Признано спамом → удалено, в историю не сохраняется; иначе —
// сохраняется (+ ответ, если адресовано боту). reason="review" — мягкий (не hard) → через LLM.
func (h *Handlers) classifyInBackground(in moderation.SpamInput, reason string, msg *models.Message, chatID int64) {
	go func() {
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
			h.answerChat(saveCtx, chatID, chatID, msg, stripBotMention(in.Text, h.botUserID), false)
		}
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
