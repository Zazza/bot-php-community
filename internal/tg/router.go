// Package tg — обёртка над go-telegram/bot: long-polling, роутер команд,
// диспетчеризация updates (message, new_chat_members, callback_query).
package tg

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Router связывает TG-updates с доменными обработчиками (moderation, chat, topics).
type Router struct {
	api      *bot.Bot
	handlers *Handlers
}

// New создаёт Router и регистрирует хендлеры.
func New(api *bot.Bot, h *Handlers) *Router {
	r := &Router{api: api, handlers: h}
	r.register()
	return r
}

// Start запускает long-polling. Блокирует до отмены ctx.
func (r *Router) Start(ctx context.Context) {
	slog.Info("tg bot starting long-polling...")
	r.api.Start(ctx)
}

func (r *Router) register() {
	// Все сообщения (текст + new_chat_members) идут через OnMessage.
	r.api.RegisterHandlerMatchFunc(
		func(u *models.Update) bool { return u.Message != nil },
		r.handlers.OnMessage,
	)
	// Callback'и кнопок модерации.
	r.api.RegisterHandlerMatchFunc(
		func(u *models.Update) bool { return u.CallbackQuery != nil },
		r.handlers.OnCallbackQuery,
	)
}

// SendMessage — helper для доменов (реализует topics.Poster). HTML с fallback на plain:
// MarkdownV2 (он же ParseModeMarkdown) отвергает неэкранированные '.', '-', '>', '_' и т.п.
// в LLM-выводе, из-за чего ответ тихо деградировал в plain без форматирования. mdToHTML
// надёжно переводит код/жирный в Telegram-HTML, всё остальное экранируется — парс не ломается.
func SendMessage(ctx context.Context, api *bot.Bot, chatID int64, text string) error {
	_, err := api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: mdToHTML(text), ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		slog.Warn("send message html failed, retry plain", "err", err)
		_, err = api.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text})
	}
	return err
}

// mdToHTML переводит подмножество markdown из ответов LLM в Telegram HTML: fenced ```code```,
// inline `code`, **bold**. Весь остальной текст проходит через html.EscapeString, поэтому
// парс не может сломаться на '.', '-', '>', '<' и прочих спецсимволах (главная боль MarkdownV2).
func mdToHTML(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		fF := strings.Index(s, "```")
		fB := strings.Index(s, "**")
		fI := strings.IndexByte(s, '`')
		pos := -1
		for _, v := range []int{fF, fB, fI} {
			if v >= 0 && (pos < 0 || v < pos) {
				pos = v
			}
		}
		if pos < 0 {
			b.WriteString(html.EscapeString(s))
			return b.String()
		}
		b.WriteString(html.EscapeString(s[:pos]))
		rest := s[pos:]
		switch {
		case strings.HasPrefix(rest, "```"):
			end := strings.Index(rest[3:], "```")
			if end < 0 {
				b.WriteString(html.EscapeString(rest))
				return b.String()
			}
			content := rest[3 : 3+end]
			if nl := strings.IndexByte(content, '\n'); nl >= 0 {
				content = content[nl+1:] // отбрасываем строку ```lang
			} else {
				content = "" // ```lang``` одной строкой → токен языка без кода
			}
			b.WriteString("<pre><code>")
			b.WriteString(html.EscapeString(content))
			b.WriteString("</code></pre>")
			s = rest[3+end+3:]
		case strings.HasPrefix(rest, "**"):
			end := strings.Index(rest[2:], "**")
			if end < 0 {
				b.WriteString(html.EscapeString(rest))
				return b.String()
			}
			b.WriteString("<b>")
			b.WriteString(html.EscapeString(rest[2 : 2+end]))
			b.WriteString("</b>")
			s = rest[2+end+2:]
		default: // одиночный '`' → inline-код
			end := strings.IndexByte(rest[1:], '`')
			if end < 0 {
				b.WriteString(html.EscapeString(rest))
				return b.String()
			}
			b.WriteString("<code>")
			b.WriteString(html.EscapeString(rest[1 : 1+end]))
			b.WriteString("</code>")
			s = rest[1+end+1:]
		}
	}
	return b.String()
}

// PosterImpl реализует topics.Poster через bot.Bot.
type PosterImpl struct{ api *bot.Bot }

// NewPoster создаёт Poster.
func NewPoster(api *bot.Bot) *PosterImpl { return &PosterImpl{api: api} }

// PostMessage отправляет текст в чат.
func (p *PosterImpl) PostMessage(ctx context.Context, chatID int64, text string) error {
	return SendMessage(ctx, p.api, chatID, text)
}

// extractCommand парсит "/cmd[@bot] args..." → (cmd_lower, args).
func extractCommand(text string) (cmd string, args string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}
	body := strings.TrimPrefix(text, "/")
	parts := strings.SplitN(body, " ", 2)
	cmd = strings.ToLower(parts[0])
	if at := strings.IndexByte(cmd, '@'); at != -1 {
		cmd = cmd[:at]
	}
	if len(parts) == 2 {
		args = strings.TrimSpace(parts[1])
	}
	return cmd, args
}

// parseInt64 безопасно парсит строку.
func parseInt64(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// replyUsername строит display-имя.
func replyUsername(u *models.User) string {
	if u == nil {
		return "user"
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		return "user"
	}
	return name
}

// replyToRef — ссылка для лога.
func replyToRef(msg *models.Message) string {
	if msg == nil || msg.ReplyToMessage == nil {
		return ""
	}
	return fmt.Sprintf("reply_to=%d", msg.ReplyToMessage.ID)
}
