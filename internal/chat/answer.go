// Package chat — PHP-assistant: ответ на /ask и упоминания через RAG-поиск по истории.
package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"phpbot/internal/faq"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/prompts"
	"phpbot/internal/websearch"
)

const (
	topK                    = 8
	recentN                 = 15
	maxAnswerLen            = 3500  // TG лимит 4096, оставляем запас
	aboutMaxChars           = 12000 // бюджет на сборку сообщений для портрета участника
	alreadyDiscussedMaxDist = 0.18  // косинусное расстояние: меньше → строже; 0.18 ≈ сходство 0.82
	faqMatchMaxDist         = 0.20  // курируемый FAQ отдаётся сразу при расстоянии вопроса ниже порога
	askRelevanceMaxDist     = 0.50  // гейт /ask: лучший RAG-матч дальше → темы в истории нет, LLM не зовётся
)

// Answerer собирает контекст (RAG + последние сообщения + веб) и зовёт LLM.
type Answerer struct {
	llm  *llm.LLMClient
	msgs *messages.Repository
	vec  *messages.VectorRepo
	web  *websearch.Searcher // nil → веб-поиск выключен
	faq  *faq.Repo           // nil → FAQ fast-path выключен
}

// New создаёт Answerer. web и faqRepo могут быть nil.
func New(llm *llm.LLMClient, msgs *messages.Repository, vec *messages.VectorRepo, web *websearch.Searcher, faqRepo *faq.Repo) *Answerer {
	return &Answerer{llm: llm, msgs: msgs, vec: vec, web: web, faq: faqRepo}
}

// Answer генерирует ответ на вопрос в контексте чата.
func (a *Answerer) Answer(ctx context.Context, chatID int64, asker, question string) (string, error) {
	q := strings.TrimSpace(question)
	if q == "" {
		return "Вопрос пустой.", nil
	}

	// 1. Векторизуем запрос. qvec==nil (ошибка) ниже триггерит гейт → отбой без LLM.
	qvec, err := a.vec.EmbedText(ctx, q)
	if err != nil {
		slog.Warn("embed query failed", "err", err)
	}

	// 1b. FAQ fast-path: курируемый ответ отдаётся сразу, минуя RAG и «Уже обсуждали».
	if qvec != nil && a.faq != nil {
		hits, ferr := a.faq.Match(ctx, chatID, qvec, faqMatchMaxDist, 1)
		if ferr != nil {
			slog.Warn("faq match failed", "err", ferr)
		} else if len(hits) > 0 {
			slog.Info("faq hit", "chat_id", chatID, "item", hits[0].ID)
			return "📌 FAQ:\n" + hits[0].Answer, nil
		}
	}

	// 2. RAG: top-K по истории чата. topSearch хранится целиком (с Distance),
	// rag собирается из него — Distance нужен ниже для prepend «Уже обсуждали».
	var topSearch []messages.SearchMessage
	var rag []messages.Message
	if qvec != nil {
		rows, err := a.vec.SearchTopK(ctx, chatID, qvec, topK)
		if err != nil {
			slog.Warn("search top-k failed", "err", err)
		} else {
			topSearch = rows
			rag = make([]messages.Message, 0, len(rows))
			for _, r := range rows {
				rag = append(rag, r.Message)
			}
		}
	}

	// 2b. Релевантность-гейт: бот отвечает только из истории чата. Если векторизации
	// нет (embed упал) или лучший матч далёк — LLM не зовём (экономия + анти-генерика),
	// отбой к живым участникам. web-поиск тоже пропускаем — он не нужен для оффтопа.
	if !relevantEnough(topSearch, askRelevanceMaxDist) {
		slog.Info("chat skip: not in chat history", "chat_id", chatID, "embed_failed", qvec == nil)
		return notInHistoryReply(qvec == nil), nil
	}

	// 3. Последние N сообщений (для актуального контекста).
	recent, err := a.msgs.Last(ctx, chatID, recentN)
	if err != nil {
		slog.Warn("last messages failed", "err", err)
	}

	// 3b. Веб-поиск — актуализация свежих фактов (версии/релизы/даты/новости).
	// Non-fatal: упал → отвечаем без веба.
	var web []websearch.Result
	if a.web != nil {
		if wr, werr := a.web.Search(ctx, q); werr != nil {
			slog.Warn("web search failed, answering without web", "err", werr)
		} else {
			web = wr
		}
	}

	// 4. Сборка контекста.
	contextBlock := buildContextBlock(rag, recent, web)
	system := prompts.Get(prompts.Chat, contextBlock)

	// 5. LLM-вызов. Атрибутируем спрашивающего, чтобы бот не путал имена из контекста.
	userMsg := q
	if asker = strings.TrimSpace(asker); asker != "" {
		userMsg = "Вопрос от " + asker + ":\n" + q
	}
	resp, inTok, outTok, err := a.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: userMsg},
	})
	if err != nil {
		return "", fmt.Errorf("chat llm: %w", err)
	}
	slog.Info("chat answer", "chat_id", chatID, "in", inTok, "out", outTok,
		"rag_len", len(rag), "recent_len", len(recent), "web_len", len(web))

	// 5b. LLM вернул SKIP — контекст прошёл гейт, но ответа в нём нет: отбой к людям.
	if isSkip(resp) {
		slog.Info("chat skip: llm returned skip", "chat_id", chatID)
		return notInHistoryReply(false), nil
	}

	// 6. «Уже обсуждали»: если на похожий вопрос уже был ответ — prepend ссылки.
	// Non-fatal: NextAfter упал → отвечаем без prepend.
	var prefix string
	if len(topSearch) > 0 && topSearch[0].Distance < alreadyDiscussedMaxDist {
		past := topSearch[0]
		quote := past.Text
		if ans, aerr := a.msgs.NextAfter(ctx, chatID, past.TS); aerr != nil {
			slog.Warn("next-after failed", "err", aerr)
		} else if ans != nil && ans.UserID != past.UserID {
			quote = ans.Text
		}
		if len(quote) > 300 {
			quote = quote[:300] + "…"
		}
		prefix = fmt.Sprintf("↩ Уже обсуждали (%s):\n%s\n\n",
			past.TS.Format("02.01.2006"), quote)
	}

	final := prefix + resp
	if len(final) > maxAnswerLen {
		final = final[:maxAnswerLen] + "\n…(обрезано)"
	}
	return final, nil
}

// About собирает краткий портрет участника по его сообщениям за период.
// Данные (тексты) идут отдельным user-сообщением — системный промпт статичен
// (анти-инъекция: инструкции участника в их тексте не исполняются).
func (a *Answerer) About(ctx context.Context, chatID int64, username string, since, until *time.Time, label string) (string, error) {
	msgs, err := a.msgs.ByUsername(ctx, chatID, username, since, until, 150)
	if err != nil {
		return "", fmt.Errorf("about fetch: %w", err)
	}
	if len(msgs) == 0 {
		return "Нет сообщений от @" + username + " за период «" + label + "».", nil
	}

	// msgs — DESC (свежие первыми). Копим свежие → старые, пока в бюджет; старые
	// сверх бюджета отбрасываем. Вывод — в хронологическом порядке (старые → свежие).
	picked := make([]string, 0, len(msgs))
	total := 0
	for _, m := range msgs {
		who := m.Username
		if who == "" {
			who = "user"
		}
		ts := m.TS.In(time.UTC).Format("2006-01-02 15:04")
		line := fmt.Sprintf("[%s] %s: %s\n", ts, who, m.Text)
		if total+len(line) > aboutMaxChars {
			break
		}
		picked = append(picked, line)
		total += len(line)
	}
	var b strings.Builder
	for i := len(picked) - 1; i >= 0; i-- {
		b.WriteString(picked[i])
	}

	system := prompts.Get(prompts.About)
	resp, inTok, outTok, err := a.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		return "", fmt.Errorf("about llm: %w", err)
	}
	slog.Info("about", "chat_id", chatID, "user", username, "msgs", len(msgs), "in", inTok, "out", outTok)

	if len(resp) > maxAnswerLen {
		resp = resp[:maxAnswerLen] + "\n…(обрезано)"
	}
	return resp, nil
}

// TopicOverview собирает краткий обзор обсуждения темы по релевантным сообщениям.
// Данные (тексты) идут отдельным user-сообщением — системный промпт статичен
// (анти-инъекция: инструкции участников в их тексте не исполняются). Нумерация [N]
// в контексте задаётся здесь и должна совпадать с нумерацией списка источников у вызывающего.
func (a *Answerer) TopicOverview(ctx context.Context, topic string, msgs []messages.Message) (string, error) {
	if len(msgs) == 0 {
		return "Нет сообщений по теме «" + topic + "».", nil
	}
	var b strings.Builder
	for i, m := range msgs {
		who := m.Username
		if who == "" {
			who = "user"
		}
		fmt.Fprintf(&b, "[%d] %s (%s): %s\n", i+1, who, m.TS.Format("02.01.06 15:04"), m.Text)
	}
	system := prompts.Get(prompts.Search)
	resp, inTok, outTok, err := a.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		return "", fmt.Errorf("search overview llm: %w", err)
	}
	slog.Info("search overview", "topic", topic, "ctx", len(msgs), "in", inTok, "out", outTok)
	if len(resp) > maxAnswerLen {
		resp = resp[:maxAnswerLen] + "\n…(обрезано)"
	}
	return resp, nil
}

// buildContextBlock формирует текстовый блок контекста для системного промпта.
func buildContextBlock(rag []messages.Message, recent []messages.Message, web []websearch.Result) string {
	var b strings.Builder
	if len(web) > 0 {
		b.WriteString("[Актуальное из веба]\n")
		b.WriteString(websearch.FormatResults(web))
		b.WriteString("\n")
	}
	if len(rag) > 0 {
		b.WriteString("[Найдено по смыслу в истории чата]\n")
		b.WriteString(messages.FormatContext(rag))
		b.WriteString("\n")
	}
	if len(recent) > 0 {
		b.WriteString("[Свежие сообщения]\n")
		b.WriteString(messages.FormatContext(recent))
	}
	if b.Len() == 0 {
		return "(история пуста — это первое обращение)"
	}
	return b.String()
}

// relevantEnough — прошёл ли лучший RAG-матч порог релевантности. False → тема в
// истории чата не обсуждалась, бот не отвечает (экономия LLM-вызова + анти-генерика).
func relevantEnough(top []messages.SearchMessage, maxDist float64) bool {
	return len(top) > 0 && top[0].Distance <= maxDist
}

// isSkip — LLM вернул SKIP (контекст прошёл гейт, но не отвечает на вопрос).
func isSkip(resp string) bool {
	return strings.EqualFold(strings.TrimSpace(resp), "SKIP")
}

// notInHistoryReply — фиксированный отбой, когда ответить из истории нельзя.
func notInHistoryReply(embedFailed bool) string {
	if embedFailed {
		return "⚠️ Не получилось проверить историю чата, попробуй ещё раз."
	}
	return "В истории чата это не обсуждали — лучше спросить у участников 🙋"
}

// FormatRecentForDigest — экспорт FormatContext-подобной утилиты для дайджеста.
func FormatRecentForDigest(msgs []messages.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		who := m.Username
		if who == "" {
			who = "user"
		}
		ts := m.TS.In(time.UTC).Format("2006-01-02 15:04")
		fmt.Fprintf(&b, "[%s] %s: %s\n", ts, who, m.Text)
	}
	return b.String()
}
