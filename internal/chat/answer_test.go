package chat

import (
	"testing"

	"phpbot/internal/messages"
)

func TestRelevantEnough(t *testing.T) {
	const maxDist = 0.50
	cases := []struct {
		name string
		top  []messages.SearchMessage
		want bool
	}{
		{"empty no history", nil, false},
		{"close match", []messages.SearchMessage{{Distance: 0.10}}, true},
		{"at threshold", []messages.SearchMessage{{Distance: 0.50}}, true},
		{"just over threshold", []messages.SearchMessage{{Distance: 0.51}}, false},
		{"far off-topic", []messages.SearchMessage{{Distance: 0.90}}, false},
		{"best close second far", []messages.SearchMessage{{Distance: 0.20}, {Distance: 0.80}}, true},
		{"best far only", []messages.SearchMessage{{Distance: 0.80}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := relevantEnough(c.top, maxDist); got != c.want {
				t.Fatalf("relevantEnough(%v, %v) = %v, want %v", c.top, maxDist, got, c.want)
			}
		})
	}
}

func TestIsSkip(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"SKIP", true},
		{"skip", true},
		{"  skip  ", true},
		{"SKIP\n", true},
		{"", false},
		{"Skip please", false},
		{"Вот ответ по теме", false},
	}
	for _, c := range cases {
		if got := isSkip(c.in); got != c.want {
			t.Fatalf("isSkip(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNotInHistoryReply(t *testing.T) {
	if got := notInHistoryReply(true); got == "" {
		t.Fatal("embedFailed reply must not be empty")
	}
	if got := notInHistoryReply(false); got == "" {
		t.Fatal("irrelevant reply must not be empty")
	}
	if notInHistoryReply(true) == notInHistoryReply(false) {
		t.Fatal("embedFailed and irrelevant replies should differ")
	}
}
