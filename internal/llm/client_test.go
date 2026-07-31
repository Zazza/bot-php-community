package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// Temperature обязан попадать в тело запроса — без него API берёт дефолт модели (~1.0)
// и ответы становятся недетерминированными (один вопрос → то SKIP, то ответ).
func TestChatRequestHasTemperature(t *testing.T) {
	cases := []struct {
		name string
		temp float64
	}{
		{"zero is sent", 0},
		{"explicit value", 0.7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewLLMClient("https://x", "k", "m", 10, tc.temp)
			b, err := json.Marshal(chatRequest{
				Model:       c.model,
				Messages:    []Message{{Role: "user", Content: "q"}},
				MaxTokens:   c.maxTokens,
				Temperature: c.temperature,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			s := string(b)
			if !strings.Contains(s, `"temperature":`) {
				t.Fatalf("temperature missing in %s", s)
			}
			if !strings.Contains(s, `"temperature":0`) && tc.temp == 0 {
				t.Fatalf("expected temperature:0 in %s", s)
			}
		})
	}
}
