// Package moderation — вторая линия модерации новичков: судья LLM + flow диалога.
package moderation

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"phpbot/internal/llm"
	"phpbot/internal/prompts"
)

// judgePrompt — промпт судьи с подставленным ответом новичка.
func judgePrompt(answer string) string {
	return prompts.Get(prompts.Judge, answer)
}

// Verdict — решение судьи.
type Verdict struct {
	Verdict string `json:"verdict"` // bot | human | unclear
	Reason  string `json:"reason"`
}

const failSafe = "unclear"

// Judge вызывает LLM-судью по ответу новичка. При любой ошибке — fail-safe "unclear":
// безопаснее позвать админа посмотреть вручную, чем авто-кикнуть живого человека.
func Judge(ctx context.Context, client *llm.LLMClient, answer string) Verdict {
	prompt := judgePrompt(answer)
	if prompt == "" {
		slog.Error("judge prompt empty")
		return Verdict{Verdict: failSafe, Reason: "judge prompt error"}
	}
	resp, _, _, err := client.Chat(ctx, []llm.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Определи verdict."},
	})
	if err != nil {
		slog.Error("judge llm call", "err", err)
		return Verdict{Verdict: failSafe, Reason: "llm error"}
	}
	return parseVerdict(resp)
}

// parseVerdict парсит JSON-ответ судьи с защитой от markdown-обёрток.
func parseVerdict(raw string) Verdict {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var v Verdict
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		slog.Warn("judge parse failed, fallback", "raw", raw, "err", err)
		return Verdict{Verdict: failSafe, Reason: "parse error"}
	}
	switch v.Verdict {
	case "bot", "human", "unclear":
		return v
	default:
		slog.Warn("judge unknown verdict", "verdict", v.Verdict)
		return Verdict{Verdict: failSafe, Reason: "unknown verdict: " + v.Verdict}
	}
}

// Question — приветственный вопрос новичку. Хардкод: формулировка стабильная,
// промпт тут избыточен (важна воспроизводимость судьи, а не вопроса).
const Question = "Привет! 👋 Расскажи в двух словах: на чём пишешь, что интересно в PHP/IT?"

// VerdictEmoji для админ-поста.
func VerdictEmoji(v string) string {
	switch v {
	case "bot":
		return "🤖"
	case "human":
		return "✅"
	case "unclear":
		return "❓"
	default:
		return "•"
	}
}
