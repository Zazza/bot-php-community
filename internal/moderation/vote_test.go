package moderation

import (
	"strconv"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

// TestDecideOutcome — "kicked" при for>=quorum и for>against, иначе "closed".
func TestDecideOutcome(t *testing.T) {
	cases := []struct {
		name         string
		forCount     int
		againstCount int
		quorum       int
		want         string
	}{
		{"kicked_above_quorum", 4, 1, 3, "kicked"},
		{"kicked_at_quorum_majority", 3, 2, 3, "kicked"},
		{"closed_for_equals_against", 3, 3, 3, "closed"},
		{"closed_below_quorum", 2, 0, 3, "closed"},
		{"closed_for_less_than_against", 5, 6, 3, "closed"},
		{"kicked_quorum_one_majority", 1, 0, 1, "kicked"},
		{"closed_quorum_one_tie", 1, 1, 1, "closed"},
		{"closed_zero_quorum_zero_tie", 0, 0, 0, "closed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideOutcome(c.forCount, c.againstCount, c.quorum)
			if got != c.want {
				t.Errorf("decideOutcome(for=%d, against=%d, quorum=%d) = %q, want %q",
					c.forCount, c.againstCount, c.quorum, got, c.want)
			}
		})
	}
}

// TestVoteKeyboard — ровно 3 кнопки с callback_data vote:<id>:{for,against,close},
// все уникальны, voteID корректно сериализован.
func TestVoteKeyboard(t *testing.T) {
	cases := []struct {
		name   string
		voteID int64
	}{
		{"small", 1},
		{"two_digit", 42},
		{"large", 9999999999},
		{"zero", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kb := voteKeyboard(c.voteID)

			var buttons []models.InlineKeyboardButton
			for _, row := range kb.InlineKeyboard {
				buttons = append(buttons, row...)
			}
			if len(buttons) != 3 {
				t.Fatalf("expected 3 buttons, got %d", len(buttons))
			}

			prefix := "vote:" + strconv.FormatInt(c.voteID, 10) + ":"
			wantCallbacks := map[string]bool{
				prefix + "for":     false,
				prefix + "against": false,
				prefix + "close":   false,
			}
			seen := make(map[string]bool, len(buttons))
			for _, b := range buttons {
				if seen[b.CallbackData] {
					t.Errorf("duplicate callback_data %q", b.CallbackData)
				}
				seen[b.CallbackData] = true
				if _, ok := wantCallbacks[b.CallbackData]; !ok {
					t.Errorf("unexpected callback_data %q", b.CallbackData)
				}
			}
			for cb := range wantCallbacks {
				if !seen[cb] {
					t.Errorf("missing callback_data %q", cb)
				}
			}

			for _, b := range buttons {
				parts := strings.Split(b.CallbackData, ":")
				if len(parts) != 3 || parts[0] != "vote" {
					t.Fatalf("callback_data %q: expected 'vote:<id>:<choice>'", b.CallbackData)
				}
				id, err := strconv.ParseInt(parts[1], 10, 64)
				if err != nil {
					t.Fatalf("callback_data %q: parse voteID: %v", b.CallbackData, err)
				}
				if id != c.voteID {
					t.Errorf("callback_data %q: voteID=%d, want %d", b.CallbackData, id, c.voteID)
				}
				if parts[2] != "for" && parts[2] != "against" && parts[2] != "close" {
					t.Errorf("callback_data %q: unknown choice %q", b.CallbackData, parts[2])
				}
			}
		})
	}
}
