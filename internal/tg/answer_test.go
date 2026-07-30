package tg

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestEnrichQuestion(t *testing.T) {
	const bot int64 = 42
	cases := []struct {
		name     string
		question string
		msg      *models.Message
		want     string
	}{
		{"no reply", "что?", &models.Message{}, "что?"},
		{"nil msg", "что?", nil, "что?"},
		{"reply to other user", "что?", &models.Message{ReplyToMessage: &models.Message{From: &models.User{ID: 99}, Text: "чужое"}}, "что?"},
		{"reply to bot adds ref", "поясни", &models.Message{ReplyToMessage: &models.Message{From: &models.User{ID: 42}, Text: "Yii3 — фреймворк"}},
			"Сообщение, на которое ответили:\nYii3 — фреймворк\n\nВопрос: поясни"},
		{"reply to bot empty question", "", &models.Message{ReplyToMessage: &models.Message{From: &models.User{ID: 42}, Text: "Yii3"}},
			"Сообщение, на которое ответили:\nYii3"},
		{"reply to bot empty ref", "что?", &models.Message{ReplyToMessage: &models.Message{From: &models.User{ID: 42}, Text: "   "}}, "что?"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := enrichQuestion(c.question, c.msg, bot); got != c.want {
				t.Fatalf("enrichQuestion(%q) = %q, want %q", c.question, got, c.want)
			}
		})
	}
}
