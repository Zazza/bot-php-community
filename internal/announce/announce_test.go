package announce

import (
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

func TestDeepLink(t *testing.T) {
	cases := []struct {
		name   string
		chatID int64
		msgID  int64
		want   string
	}{
		{"supergroup", -1001234567890, 42, "https://t.me/c/1234567890/42"},
		{"supergroup small", -1000000000123, 7, "https://t.me/c/123/7"},
		{"public group no link", -100, 5, ""},        // нет -100-префикса полной длины
		{"positive chat", 123456, 5, ""},              // личка/публичная — не строим
		{"zero msg", -1001234567890, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deepLink(c.chatID, c.msgID); got != c.want {
				t.Errorf("deepLink(%d,%d) = %q, want %q", c.chatID, c.msgID, got, c.want)
			}
		})
	}
}

func TestReplyName(t *testing.T) {
	cases := []struct {
		name string
		u    models.User
		want string
	}{
		{"username", models.User{ID: 1, Username: "ivan", FirstName: "Иван"}, "@ivan"},
		{"first last", models.User{ID: 2, FirstName: "Иван", LastName: "Петров"}, "Иван Петров"},
		{"first only", models.User{ID: 3, FirstName: "Ольга"}, "Ольга"},
		{"empty", models.User{ID: 4}, "user_4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := replyName(c.u); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestKeyboard(t *testing.T) {
	kb := keyboard(123)
	if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatalf("expected single button, got %+v", kb.InlineKeyboard)
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.Text != buttonLabel {
		t.Errorf("label = %q, want %q", btn.Text, buttonLabel)
	}
	if btn.CallbackData != "announce:123" {
		t.Errorf("callback = %q, want announce:123", btn.CallbackData)
	}
}

func TestPostRef(t *testing.T) {
	t.Run("accessible message", func(t *testing.T) {
		cb := &models.CallbackQuery{
			Data: "announce:1",
			Message: models.MaybeInaccessibleMessage{
				Type:    models.MaybeInaccessibleMessageTypeMessage,
				Message: &models.Message{ID: 42, Chat: models.Chat{ID: -1001234567890}},
			},
		}
		chatID, msgID := postRef(cb)
		if chatID != -1001234567890 || msgID != 42 {
			t.Errorf("got (%d,%d), want (-1001234567890,42)", chatID, msgID)
		}
	})
	t.Run("inaccessible message", func(t *testing.T) {
		cb := &models.CallbackQuery{
			Data: "announce:1",
			Message: models.MaybeInaccessibleMessage{
				Type:                models.MaybeInaccessibleMessageTypeInaccessibleMessage,
				InaccessibleMessage: &models.InaccessibleMessage{MessageID: 99, Chat: models.Chat{ID: -100111222333}},
			},
		}
		chatID, msgID := postRef(cb)
		if chatID != -100111222333 || msgID != 99 {
			t.Errorf("got (%d,%d), want (-100111222333,99)", chatID, msgID)
		}
	})
	t.Run("nil cb", func(t *testing.T) {
		if chatID, msgID := postRef(nil); chatID != 0 || msgID != 0 {
			t.Errorf("nil cb should give zeros, got (%d,%d)", chatID, msgID)
		}
	})
}

func TestBuildNotify(t *testing.T) {
	t.Run("valid with link", func(t *testing.T) {
		cb := &models.CallbackQuery{
			Data: "announce:777",
			From: models.User{ID: 10, Username: "speaker"},
			Message: models.MaybeInaccessibleMessage{
				Type:    models.MaybeInaccessibleMessageTypeMessage,
				Message: &models.Message{ID: 42, Chat: models.Chat{ID: -1001234567890}},
			},
		}
		adminID, text, ok := buildNotify(cb)
		if !ok {
			t.Fatal("expected ok")
		}
		if adminID != 777 {
			t.Errorf("adminID = %d, want 777", adminID)
		}
		if !strings.Contains(text, "@speaker") {
			t.Errorf("text must mention @speaker: %q", text)
		}
		if !strings.Contains(text, "https://t.me/c/1234567890/42") {
			t.Errorf("text must contain post link: %q", text)
		}
	})
	t.Run("valid no username", func(t *testing.T) {
		cb := &models.CallbackQuery{
			Data: "announce:5",
			From: models.User{ID: 10, FirstName: "Ольга"},
			Message: models.MaybeInaccessibleMessage{
				Type:    models.MaybeInaccessibleMessageTypeMessage,
				Message: &models.Message{ID: 1, Chat: models.Chat{ID: -100}}, // публичная — без ссылки
			},
		}
		_, text, ok := buildNotify(cb)
		if !ok {
			t.Fatal("expected ok")
		}
		if !strings.Contains(text, "Ольга") {
			t.Errorf("text must contain name: %q", text)
		}
		if strings.Contains(text, "t.me") {
			t.Errorf("public group must have no link: %q", text)
		}
	})
	t.Run("wrong prefix", func(t *testing.T) {
		cb := &models.CallbackQuery{Data: "quiz:1:0"}
		if _, _, ok := buildNotify(cb); ok {
			t.Fatal("non-announce data must not parse")
		}
	})
	t.Run("bad admin id", func(t *testing.T) {
		for _, data := range []string{"announce:abc", "announce:0", "announce:-1", "announce:"} {
			cb := &models.CallbackQuery{Data: data}
			if _, _, ok := buildNotify(cb); ok {
				t.Errorf("data %q must not parse", data)
			}
		}
	})
	t.Run("nil cb", func(t *testing.T) {
		if _, _, ok := buildNotify(nil); ok {
			t.Fatal("nil cb must not parse")
		}
	})
}

func TestState(t *testing.T) {
	s := New(nil, nil, 0)
	if s.active(1) {
		t.Fatal("no state expected initially")
	}
	s.setState(1)
	if !s.active(1) {
		t.Fatal("state expected after setState")
	}
	if !s.Cancel(1) {
		t.Fatal("Cancel should report was-active")
	}
	if s.active(1) {
		t.Fatal("state must be cleared after Cancel")
	}
	if s.Cancel(1) {
		t.Fatal("Cancel of empty must be false")
	}

	// lazy-expiry: истёкшая запись инактивна и вычищается.
	s.setState(2)
	s.mu.Lock()
	s.pending[2] = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if s.active(2) {
		t.Fatal("expired state must be inactive")
	}
	s.mu.Lock()
	_, present := s.pending[2]
	s.mu.Unlock()
	if present {
		t.Fatal("expired state must be cleaned up")
	}
}

func TestIsAdmin(t *testing.T) {
	s := New(nil, []int64{10, 20}, 0)
	if !s.isAdmin(10) || !s.isAdmin(20) {
		t.Error("10 and 20 must be admins")
	}
	if s.isAdmin(30) {
		t.Error("30 must not be admin")
	}
	// пустой список админов — никто не админ (анонсы тогда не создать через cmdAnnounce)
	if New(nil, nil, 0).isAdmin(1) {
		t.Error("empty adminIDs: nobody is admin")
	}
}

func newClickCB(msgID, clicker int64, adminData string) *models.CallbackQuery {
	return &models.CallbackQuery{
		Data: adminData,
		From: models.User{ID: clicker, Username: "u"},
		Message: models.MaybeInaccessibleMessage{
			Type:    models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: int(msgID), Chat: models.Chat{ID: -1001234567890}},
		},
	}
}

func TestMarkNotifiedDedup(t *testing.T) {
	s := New(nil, []int64{1}, 0)

	// первый клик юзера 7 по посту 42 — не дубликат, регистрируем
	if s.markNotified(newClickCB(42, 7, "announce:1")) {
		t.Fatal("first click must not be dup")
	}
	// повторный клик того же юзера по тому же посту — дубликат
	if !s.markNotified(newClickCB(42, 7, "announce:1")) {
		t.Fatal("second click on same post must be dup")
	}
	// другой юзер по тому же посту — не дубликат
	if s.markNotified(newClickCB(42, 8, "announce:1")) {
		t.Fatal("different clicker must not be dup")
	}
	// тот же юзер по другому посту — не дубликат
	if s.markNotified(newClickCB(43, 7, "announce:1")) {
		t.Fatal("different post must not be dup")
	}
}

func TestMarkNotifiedExpiry(t *testing.T) {
	s := New(nil, []int64{1}, 0)
	cb := newClickCB(42, 7, "announce:1")
	if s.markNotified(cb) {
		t.Fatal("first must not be dup")
	}
	// искусственно состарим запись → истёк dedupTTL, повторный клик снова «первый»
	s.mu.Lock()
	s.notified[dedupKey(cb)] = time.Now().Add(-dedupTTL - time.Second)
	s.mu.Unlock()
	if s.markNotified(cb) {
		t.Fatal("after TTL expiry click must not be dup")
	}
}
