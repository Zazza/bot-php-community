// Package tg — обёртка над go-telegram/bot: long-polling, роутер команд,
// диспетчеризация updates (message, new_chat_members, callback_query).
package tg

import (
	"context"
	"fmt"
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

// SendMessage — helper для доменов (реализует topics.Poster). Markdown с fallback на plain.
func SendMessage(ctx context.Context, api *bot.Bot, chatID int64, text string) error {
	_, err := api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: text, ParseMode: models.ParseModeMarkdown,
	})
	if err != nil {
		slog.Warn("send message markdown failed, retry plain", "err", err)
		_, err = api.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text})
	}
	return err
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
