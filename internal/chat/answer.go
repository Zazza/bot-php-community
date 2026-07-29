// Package chat — PHP-assistant: ответ на /ask и упоминания через RAG-поиск по истории.
package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/prompts"
)

const (
	topK         = 8
	recentN      = 15
	maxAnswerLen = 3500 // TG лимит 4096, оставляем запас
)

// Answerer собирает контекст (RAG + последние сообщения) и зовёт LLM.
type Answerer struct {
	llm   *llm.LLMClient
	msgs  *messages.Repository
	vec   *messages.VectorRepo
}

// New создаёт Answerer.
func New(llm *llm.LLMClient, msgs *messages.Repository, vec *messages.VectorRepo) *Answerer {
	return &Answerer{llm: llm, msgs: msgs, vec: vec}
}

// Answer генерирует ответ на вопрос в контексте чата.
func (a *Answerer) Answer(ctx context.Context, chatID int64, asker, question string) (string, error) {
	q := strings.TrimSpace(question)
	if q == "" {
		return "Вопрос пустой.", nil
	}

	// 1. Векторизуем запрос.
	qvec, err := a.vec.EmbedText(ctx, q)
	if err != nil {
		slog.Warn("embed query failed, answering without RAG", "err", err)
	}

	// 2. RAG: top-K по истории чата.
	var rag []messages.Message
	if qvec != nil {
		rows, err := a.vec.SearchTopK(ctx, chatID, qvec, topK)
		if err != nil {
			slog.Warn("search top-k failed", "err", err)
		} else {
			rag = make([]messages.Message, 0, len(rows))
			for _, r := range rows {
				rag = append(rag, r.Message)
			}
		}
	}

	// 3. Последние N сообщений (для актуального контекста).
	recent, err := a.msgs.Last(ctx, chatID, recentN)
	if err != nil {
		slog.Warn("last messages failed", "err", err)
	}

	// 4. Сборка контекста.
	contextBlock := buildContextBlock(rag, recent)
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
		"rag_len", len(rag), "recent_len", len(recent))

	if len(resp) > maxAnswerLen {
		resp = resp[:maxAnswerLen] + "\n…(обрезано)"
	}
	return resp, nil
}

// buildContextBlock формирует текстовый блок контекста для системного промпта.
func buildContextBlock(rag []messages.Message, recent []messages.Message) string {
	var b strings.Builder
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
