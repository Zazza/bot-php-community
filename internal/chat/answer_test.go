package chat

import "testing"

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

func TestSourceLink(t *testing.T) {
	cases := []struct {
		chatID, msgID int64
		want          string
	}{
		{-1001120236018, 12345, "https://t.me/c/1120236018/12345"},
		{-1001120236018, 1, "https://t.me/c/1120236018/1"},
	}
	for _, c := range cases {
		if got := sourceLink(c.chatID, c.msgID); got != c.want {
			t.Fatalf("sourceLink(%d,%d) = %q, want %q", c.chatID, c.msgID, got, c.want)
		}
	}
}
