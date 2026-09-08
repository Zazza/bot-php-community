// Package tg — обёртка над go-telegram/bot: long-polling, роутер команд,
// диспетчеризация updates (message, new_chat_members, callback_query).
package tg

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"phpbot/internal/md"
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
// в LLM-выводе, из-за чего ответ тихо деградировал в plain без форматирования. md.ToHTML
// надёжно переводит код/жирный в Telegram-HTML, всё остальное экранируется — парс не ломается.
func SendMessage(ctx context.Context, api *bot.Bot, chatID int64, text string) error {
	_, err := api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: md.ToHTML(text), ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		slog.Warn("send message html failed, retry plain", "err", err)
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

// PostPhotos отправляет альбом (sendMediaGroup): по одному фото на путь, caption —
// только на первом (TG показывает подпись альбома один раз).
func (p *PosterImpl) PostPhotos(ctx context.Context, chatID int64, paths []string, caption string) error {
	files := make([]*os.File, 0, len(paths))
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	media := make([]models.InputMedia, 0, len(paths))
	for i, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open photo %s: %w", path, err)
		}
		files = append(files, f)
		photo := &models.InputMediaPhoto{Media: "attach://p" + strconv.Itoa(i), MediaAttachment: f}
		if i == 0 {
			photo.Caption = caption
		}
		media = append(media, photo)
	}
	if _, err := p.api.SendMediaGroup(ctx, &bot.SendMediaGroupParams{ChatID: chatID, Media: media}); err != nil {
		return fmt.Errorf("send media group: %w", err)
	}
	return nil
}

// PostDocument отправляет файл документом (без пересжатия TG). Detection выключен:
// содержимое — PNG, но как документ оно не должно переквалифицироваться в фото.
func (p *PosterImpl) PostDocument(ctx context.Context, chatID int64, path, filename string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open document %s: %w", path, err)
	}
	defer f.Close()
	if _, err := p.api.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:                      chatID,
		Document:                    &models.InputFileUpload{Filename: filename, Data: f},
		DisableContentTypeDetection: true,
	}); err != nil {
		return fmt.Errorf("send document: %w", err)
	}
	return nil
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
