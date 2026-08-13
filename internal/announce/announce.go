// Package announce — админская рассылка анонса митапа. Команда /announce в личке бота
// переводит админа в режим ожидания текста; следующий текст постится в основной чат с
// inline-кнопкой «Хочу выступить». Клик по кнопке шлёт уведомление админу, который
// запустил анонс (callback_data несёт его user_id).
package announce

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"phpbot/internal/md"
)

const (
	// stateTimeout — сколько ждём текст анонса после /announce.
	stateTimeout = 5 * time.Minute

	// dedupTTL — окно дедупликации кликов: повторные нажатия того же юзера на ту же
	// кнопку анонса в пределах окна не плодят уведомления админу (anti-abuse).
	dedupTTL = 24 * time.Hour

	callbackPrefix = "announce:"
	buttonLabel    = "Хочу выступить 🙋"

	startPrompt = "📝 Пришли текст анонса одним сообщением. Можно с разметкой (**жирный**, `код`). /cancel — отмена."
	emptyText   = "Текст пустой. Пришли текст анонса или /cancel — отмена."
	CancelText  = "❌ Ввод анонса отменён."
	postFail    = "Не удалось запостить анонс, попробуй позже."

	// clickToast — ответ тостом тому, кто нажал кнопку впервые.
	clickToast = "Готово! Я передал организаторам — с тобой свяжутся 🙂"
	dupToast   = "Ты уже отмечался — организаторы в курсе 🙂"
)

// Service хранит состояние команды /announce и обрабатывает клики по кнопке анонса.
type Service struct {
	api          *bot.Bot
	adminIDs     map[int64]struct{}
	targetChatID int64

	mu       sync.Mutex
	pending  map[int64]time.Time  // admin_id → дедлайн режима ожидания текста
	notified map[string]time.Time // "<msgID>:<clickerID>" → время отправки уведомления
}

// New создаёт Service. adminIDs — кому разрешён /announce и кому уходят отклики;
// targetChatID — основной чат, куда постятся анонсы.
func New(api *bot.Bot, adminIDs []int64, targetChatID int64) *Service {
	ids := make(map[int64]struct{}, len(adminIDs))
	for _, id := range adminIDs {
		ids[id] = struct{}{}
	}
	return &Service{
		api:          api,
		adminIDs:     ids,
		targetChatID: targetChatID,
		pending:      make(map[int64]time.Time),
		notified:     make(map[string]time.Time),
	}
}

// Start переводит админа в режим ожидания текста анонса.
func (s *Service) Start(ctx context.Context, replyChatID, adminID int64) {
	s.setState(adminID)
	_ = send(ctx, s.api, replyChatID, startPrompt)
}

// ConsumeText перехватывает текст админа, если он в режиме ожидания анонса. Возвращает
// true, если сообщение обработано (стало анонсом или отклонено как пустое) — тогда
// вызывающий не должен отдавать его в /ask; false — режим не активен, идём стандартным путём.
func (s *Service) ConsumeText(ctx context.Context, adminID int64, text string, replyChatID int64) bool {
	text = strings.TrimSpace(text)

	// Check-and-delete под одним локом: исключает TOCTOU (двойной пост при двух сообщениях
	// подряд, если бы updates раздавались конкурентно). Пустой текст state не сбрасывает.
	s.mu.Lock()
	dl, active := s.pending[adminID]
	if active && time.Now().After(dl) {
		delete(s.pending, adminID)
		active = false
	}
	if active && text != "" {
		delete(s.pending, adminID)
	}
	s.mu.Unlock()

	if !active {
		return false
	}
	if text == "" {
		_ = send(ctx, s.api, replyChatID, emptyText)
		return true
	}

	sent, err := s.postAnnouncement(ctx, text, adminID)
	if err != nil {
		slog.Error("announce post", "err", err, "chat_id", s.targetChatID)
		_ = send(ctx, s.api, replyChatID, postFail)
		return true
	}
	slog.Info("announce posted", "chat_id", s.targetChatID, "msg_id", sent.ID, "admin_id", adminID)
	if link := deepLink(s.targetChatID, int64(sent.ID)); link != "" {
		_ = send(ctx, s.api, replyChatID, "✅ Запостил в чат: "+link)
	} else {
		_ = send(ctx, s.api, replyChatID, "✅ Запостил в чат.")
	}
	return true
}

// Cancel сбрасывает режим ожидания. Возвращает, был ли он активен.
func (s *Service) Cancel(adminID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[adminID]; !ok {
		return false
	}
	delete(s.pending, adminID)
	return true
}

// HandleCallback обрабатывает клик по кнопке анонса: шлёт запустившему админу в личку
// уведомление «<имя> хочет выступить» + ссылку на пост. Повторные клики того же юзера на
// тот же анонс (в пределах dedupTTL) дедуплицируются. Возвращает тост для нажавшего
// (пусто — некорректный callback, тогда вызывающий шлёт нейтральный «-»).
func (s *Service) HandleCallback(ctx context.Context, cb *models.CallbackQuery) string {
	adminID, text, ok := buildNotify(cb)
	if !ok || !s.isAdmin(adminID) {
		return ""
	}
	if s.markNotified(cb) {
		return dupToast
	}
	if _, err := s.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: adminID, Text: text}); err != nil {
		// Бот не может писать первым: если админ не сделал боту /start, SendMessage упадёт
		// 403. Логируем, не падаем — нажавший всё равно получит тост.
		slog.Warn("announce notify admin", "err", err, "admin_id", adminID, "from", cb.From.ID)
	}
	slog.Info("announce volunteer", "admin_id", adminID, "from", cb.From.ID)
	return clickToast
}

// postAnnouncement шлёт анонс в основной чат с кнопкой. Сначала HTML (md.ToHTML); если
// Telegram отвергнет разметку (напр. запрещённая схема URL в ссылке) — ретрай plain-текстом,
// кнопка та же. Разметка теряется, но анонс доходит.
func (s *Service) postAnnouncement(ctx context.Context, text string, adminID int64) (*models.Message, error) {
	params := &bot.SendMessageParams{
		ChatID:      s.targetChatID,
		Text:        md.ToHTML(text),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: keyboard(adminID),
	}
	sent, err := s.api.SendMessage(ctx, params)
	if err == nil {
		return sent, nil
	}
	slog.Warn("announce post html failed, retry plain", "err", err)
	params.Text = text
	params.ParseMode = ""
	return s.api.SendMessage(ctx, params)
}

// buildNotify парсит callback_data кнопки и строит текст уведомления для админа.
// callback_data: announce:<admin_id>; chat/msg поста берутся из самого callback-сообщения.
func buildNotify(cb *models.CallbackQuery) (adminID int64, text string, ok bool) {
	if cb == nil {
		return 0, "", false
	}
	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, "", false
	}
	chatID, msgID := postRef(cb)
	var b strings.Builder
	fmt.Fprintf(&b, "🙋 %s хочет выступить", replyName(cb.From))
	if link := deepLink(chatID, msgID); link != "" {
		fmt.Fprintf(&b, " по анонсу: %s", link)
	}
	return id, b.String(), true
}

// postRef достаёт chat_id и message_id поста-анонса из callback-сообщения. Для
// недоступных (inaccessible) сообщений message_id всё равно известен.
func postRef(cb *models.CallbackQuery) (chatID int64, msgID int64) {
	if cb == nil {
		return 0, 0
	}
	mim := cb.Message
	switch mim.Type {
	case models.MaybeInaccessibleMessageTypeMessage:
		if mim.Message != nil {
			return mim.Message.Chat.ID, int64(mim.Message.ID)
		}
	case models.MaybeInaccessibleMessageTypeInaccessibleMessage:
		if mim.InaccessibleMessage != nil {
			return mim.InaccessibleMessage.Chat.ID, int64(mim.InaccessibleMessage.MessageID)
		}
	}
	return 0, 0
}

// setState включает режим ожидания текста для админа.
func (s *Service) setState(adminID int64) {
	s.mu.Lock()
	s.pending[adminID] = time.Now().Add(stateTimeout)
	s.mu.Unlock()
}

// active — ждёт ли админ текста (с lazy-чисткой истёкших записей). state ограничен числом
// админов, поэтому отдельный sweeper не нужен: истёкшая запись вычищается при обращении.
func (s *Service) active(adminID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	dl, ok := s.pending[adminID]
	if !ok {
		return false
	}
	if time.Now().After(dl) {
		delete(s.pending, adminID)
		return false
	}
	return true
}

// isAdmin — входит ли user_id в список админов (defense-in-depth: callback_data теоретически
// не подделывается, но проверяем, что адресат уведомления действительно админ).
func (s *Service) isAdmin(id int64) bool {
	_, ok := s.adminIDs[id]
	return ok
}

// markNotified регистрирует отклик кликера по анонсу. Возвращает true, если этот юзер уже
// отмечался по этому посту в пределах dedupTTL (дубликат — не уведомляем админа повторно).
func (s *Service) markNotified(cb *models.CallbackQuery) bool {
	key := dedupKey(cb)
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.notified[key]; ok && time.Since(t) < dedupTTL {
		return true
	}
	s.notified[key] = time.Now()
	return false
}

// dedupKey — ключ дедупликации: конкретный пост-анонс + кликер. Юзер может отметить на
// разных анонсах (разные msgID), но не заспамить один пост.
func dedupKey(cb *models.CallbackQuery) string {
	_, msgID := postRef(cb)
	return strconv.FormatInt(msgID, 10) + ":" + strconv.FormatInt(cb.From.ID, 10)
}

// keyboard — inline-кнопка «Хочу выступить». callback_data: announce:<admin_id>.
func keyboard(adminID int64) models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: buttonLabel, CallbackData: callbackPrefix + strconv.FormatInt(adminID, 10)},
	}}}
}

// replyName — отображаемое имя пользователя для уведомления админу.
func replyName(u models.User) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		return fmt.Sprintf("user_%d", u.ID)
	}
	return name
}

// deepLink строит прямую ссылку на сообщение приватной супергруппы/канала:
// https://t.me/c/<internal_id>/<msg_id>, где internal_id = chat_id без префикса -100.
// Для публичных групп (нет -100-префикса) ссылку построить нельзя без username чата → "".
func deepLink(chatID int64, msgID int64) string {
	if msgID == 0 {
		return ""
	}
	if chatID <= -1000000000000 { // -100<internal_id>
		return fmt.Sprintf("https://t.me/c/%d/%d", -chatID-1000000000000, msgID)
	}
	return ""
}

// send шлёт служебное сообщение (plain, без markdown-рендера — служебные тексты бота).
func send(ctx context.Context, api *bot.Bot, chatID int64, text string) error {
	_, err := api.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text})
	return err
}
