package moderation

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/prompts"
	"phpbot/internal/users"
)

// OnNewMember — гейт новичка: мьют + капча (математика, inline-кнопки).
func (f *Flow) OnNewMember(ctx context.Context, chatID, userID int64, username string) {
	if !f.gateEnabled {
		_, _ = f.users.Upsert(ctx, &users.User{TGUserID: userID, Username: username, Status: "member"})
		return
	}
	display := atUser(username, "новичок")
	if _, err := f.users.Upsert(ctx, &users.User{TGUserID: userID, Username: username, Status: "suspect"}); err != nil {
		slog.Error("gate upsert suspect", "err", err)
	}
	// мьют до решения капчи. Muted-юзер всё ещё может нажать inline-кнопку.
	if err := f.mute(ctx, chatID, userID); err != nil {
		slog.Error("gate mute", "err", err)
	}

	expr, options, correct := genCaptcha()
	deadline := time.Now().Add(f.captchaTO)
	gateID, err := f.repo.CreateGate(ctx, chatID, userID, username, correct, deadline)
	if err != nil {
		slog.Error("gate create", "err", err)
		return
	}
	text := fmt.Sprintf("Привет, %s! 👋 Подтверди, что не бот — сколько будет %s?", display, expr)
	sent, err := f.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: text, ReplyMarkup: gateKeyboard(gateID, options),
	})
	if err != nil {
		slog.Error("gate send captcha", "err", err)
		return
	}
	if sent != nil {
		_ = f.repo.SetCaptchaMessage(ctx, gateID, int64(sent.ID))
	}
}

// HandleGateCallback — тап по варианту капчи. Возвращает текст toast (ephemeral).
func (f *Flow) HandleGateCallback(ctx context.Context, cb *models.CallbackQuery) string {
	if cb.From.ID == 0 {
		return ""
	}
	parts := strings.Split(cb.Data, ":") // gate:<id>:<opt>
	if len(parts) != 3 {
		return "Некорректная кнопка"
	}
	gateID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "Некорректная кнопка"
	}
	opt, err := strconv.Atoi(parts[2])
	if err != nil {
		return "Некорректная кнопка"
	}
	g, err := f.repo.GetGate(ctx, gateID)
	if err != nil || g == nil {
		return "Проверка уже не активна"
	}
	if g.State != "pending" {
		return "Проверка уже завершена"
	}

	if opt == g.CorrectOption {
		// Верно → размьют до «только текст» (линк-провация).
		if err := f.restrictTextOnly(ctx, g.ChatID, g.TGUserID); err != nil {
			slog.Error("gate restrict text-only", "err", err)
		}
		_ = f.repo.SetGateSolved(ctx, gateID, time.Now().Add(f.probation))
		_ = f.users.SetStatus(ctx, g.TGUserID, "member")
		newText := fmt.Sprintf("✅ %s — верно! Пиши, но без ссылок/медиа первые %d ч (анти-спам).",
			atUser(g.Username, "новичок"), int(f.probation.Hours()))
		f.editAndClear(ctx, g.ChatID, g.CaptchaMessageID, newText)
		go f.postSmartWelcome(g.ChatID, g.Username)
		return "✅ Верно!"
	}

	// Неверно.
	attempts, _ := f.repo.IncAttempts(ctx, gateID)
	if attempts >= f.maxAttempts {
		_ = f.kickUser(ctx, g.ChatID, g.TGUserID)
		_ = f.repo.SetGateKicked(ctx, gateID)
		_ = f.users.SetStatus(ctx, g.TGUserID, "banned")
		f.deleteCaptchaMessage(ctx, g.ChatID, g.CaptchaMessageID)
		return "❌ Неверно, лимит попыток исчерпан — пока!"
	}
	return "❌ Неверно, попробуй ещё"
}

// OnLeftMember — новичок ушёл/кикнут (напр. Combot снёс первым): убрать orphan-капчу.
func (f *Flow) OnLeftMember(ctx context.Context, chatID, userID int64) {
	g, err := f.repo.PendingGateForUser(ctx, chatID, userID)
	if err != nil || g == nil {
		return
	}
	f.deleteCaptchaMessage(ctx, chatID, g.CaptchaMessageID)
	_ = f.repo.SetGateCancelled(ctx, g.ID)
	slog.Info("gate cleanup on leave", "user", userID)
}

// Start запускает свипер гейта: кик по таймауту, cleanup ушедших, снятие провации.
func (f *Flow) Start(ctx context.Context) {
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-f.stop:
				return
			case <-t.C:
				f.sweep(ctx)
			}
		}
	}()
	slog.Info("moderation gate sweeper started")
}

// Stop дожидается свипера.
func (f *Flow) Stop() {
	close(f.stop)
	f.wg.Wait()
}

func (f *Flow) sweep(ctx context.Context) {
	now := time.Now()
	// 1. Просроченные pending → кик + удалить капчу.
	if due, err := f.repo.PendingDue(ctx, now); err == nil {
		for _, g := range due {
			_ = f.kickUser(ctx, g.ChatID, g.TGUserID)
			f.deleteCaptchaMessage(ctx, g.ChatID, g.CaptchaMessageID)
			_ = f.repo.SetGateKicked(ctx, g.ID)
			_ = f.users.SetStatus(ctx, g.TGUserID, "banned")
			slog.Info("gate timeout kick", "user", g.TGUserID)
		}
	}
	// 2. Cleanup: pending, чей дедлайн ещё не горит, но юзер уже ушёл (getChatMember fallback).
	if pending, err := f.repo.PendingNotDue(ctx, now); err == nil {
		for _, g := range pending {
			if f.userGone(ctx, g.ChatID, g.TGUserID) {
				f.deleteCaptchaMessage(ctx, g.ChatID, g.CaptchaMessageID)
				_ = f.repo.SetGateCancelled(ctx, g.ID)
				slog.Info("gate cleanup: user gone", "user", g.TGUserID)
			}
		}
	}
	// 3. Снять линк-провацию по истечении.
	if prob, err := f.repo.ProbationDue(ctx, now); err == nil {
		for _, g := range prob {
			_ = f.unmuteFull(ctx, g.ChatID, g.TGUserID)
			_ = f.repo.SetGateReleased(ctx, g.ID)
			slog.Info("gate probation released", "user", g.TGUserID)
		}
	}
}

// --- restrict / message helpers ---

func (f *Flow) mute(ctx context.Context, chatID, userID int64) error {
	return muteUser(ctx, f.api, chatID, userID)
}

func (f *Flow) restrictTextOnly(ctx context.Context, chatID, userID int64) error {
	return restrictUserTextOnly(ctx, f.api, chatID, userID)
}

func (f *Flow) unmuteFull(ctx context.Context, chatID, userID int64) error {
	return unmuteUserFull(ctx, f.api, chatID, userID)
}

func (f *Flow) deleteCaptchaMessage(ctx context.Context, chatID, messageID int64) {
	deleteChatMessage(ctx, f.api, chatID, messageID)
}

// editAndClear правит текст капчи и убирает кнопки.
func (f *Flow) editAndClear(ctx context.Context, chatID, messageID int64, text string) {
	if messageID == 0 {
		return
	}
	_, err := f.api.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:      chatID,
		MessageID:   int(messageID),
		Text:        text,
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}},
	})
	if err != nil {
		slog.Warn("gate edit message", "err", err)
	}
}

// postSmartWelcome асинхронно постит новичку «сейчас обсуждают: <3 темы>» по недавним
// сообщениям чата. Запускается горутиной из solved-ветки, чтобы не тормозить callback.
// Fail-safe: любая ошибка/пустой ответ → тихий пропуск (капча уже поприветствовала).
func (f *Flow) postSmartWelcome(chatID int64, username string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	recent, err := f.msgs.Last(ctx, chatID, 40)
	if err != nil || len(recent) == 0 {
		slog.Warn("smart welcome: no recent messages", "err", err)
		return
	}
	system := prompts.Get(prompts.Welcome)
	resp, _, _, err := f.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: messages.FormatContext(recent)},
	})
	if err != nil {
		slog.Warn("smart welcome llm", "err", err)
		return
	}
	resp = strings.TrimSpace(resp)
	if resp == "" {
		return
	}
	name := atUser(username, "новичок")
	text := "🚀 " + name + ", добро пожаловать в «PHP-сообщество Воронеж»!\n\nСейчас тут обсуждают:\n" + resp
	if _, err := f.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		slog.Warn("smart welcome post", "err", err)
	}
}

func (f *Flow) userGone(ctx context.Context, chatID, userID int64) bool {
	return isUserGone(ctx, f.api, chatID, userID)
}

func atUser(username, fallback string) string {
	if username == "" {
		return fallback
	}
	return "@" + username
}

// genCaptcha возвращает выражение, 4 варианта ответа и индекс верного.
func genCaptcha() (expr string, options []string, correct int) {
	a := rand.Intn(9) + 1
	b := rand.Intn(9) + 1
	if a < b {
		a, b = b, a
	}
	var answer int
	if rand.Intn(2) == 0 {
		answer = a + b
		expr = fmt.Sprintf("%d + %d", a, b)
	} else {
		answer = a - b
		expr = fmt.Sprintf("%d − %d", a, b)
	}
	set := map[int]struct{}{answer: {}}
	for len(set) < 4 {
		d := answer + (rand.Intn(7) - 3) // -3..3
		if d < 0 || d == answer {
			d = answer + rand.Intn(4) + 1
		}
		set[d] = struct{}{}
	}
	nums := make([]int, 0, len(set))
	for n := range set {
		nums = append(nums, n)
	}
	rand.Shuffle(len(nums), func(i, j int) { nums[i], nums[j] = nums[j], nums[i] })
	options = make([]string, len(nums))
	for i, n := range nums {
		options[i] = strconv.Itoa(n)
		if n == answer {
			correct = i
		}
	}
	return expr, options, correct
}

// gateKeyboard — варианты капчи, layout 2 в ряд. callback_data: gate:<id>:<opt>.
func gateKeyboard(gateID int64, options []string) models.InlineKeyboardMarkup {
	prefix := "gate:" + strconv.FormatInt(gateID, 10) + ":"
	btns := make([]models.InlineKeyboardButton, len(options))
	for i, o := range options {
		btns[i] = models.InlineKeyboardButton{Text: o, CallbackData: prefix + strconv.Itoa(i)}
	}
	rows := make([][]models.InlineKeyboardButton, 0, (len(btns)+1)/2)
	for i := 0; i < len(btns); i += 2 {
		end := i + 2
		if end > len(btns) {
			end = len(btns)
		}
		rows = append(rows, btns[i:end])
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}
