package tg

import (
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestIsTelegramHandle(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"dsamotoy", true},
		{"ramzych", true},
		{"BeMySlaveDarlin", true},
		{"user_123", true},
		{"", false},
		{"user", false},                 // 4 символа < 5
		{"Андрей Богачевский", false},   // пробел + не-ASCII
		{"Dmitry Dsmotoy", false},       // пробел
		{"Андрей", false},               // не-ASCII
		{"with space", false},
		{"bad-char!", false},
		{"this_handle_is_way_too_long_to_be_valid_xxxxxx", false}, // >32
	}
	for _, c := range cases {
		if got := isTelegramHandle(c.in); got != c.want {
			t.Fatalf("isTelegramHandle(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestExpertLine(t *testing.T) {
	cases := []struct {
		name       string
		i          int
		userID     int64
		username   string
		count      int
		wantSubstr string
	}{
		{"real handle gets @", 1, 42, "dsamotoy", 3, ">@dsamotoy</a>"},
		{"display name no @", 2, 43, "Андрей Богачевский", 1, ">Андрей Богачевский</a>"},
		{"display name with space no @", 3, 44, "Dmitry Dsmotoy", 2, ">Dmitry Dsmotoy</a>"},
		{"empty falls back to user", 4, 45, "", 1, ">user</a>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := expertLine(c.i, c.userID, c.username, c.count)
			if !strings.Contains(got, c.wantSubstr) {
				t.Fatalf("expertLine(%d,%d,%q,%d) = %q, want substring %q",
					c.i, c.userID, c.username, c.count, got, c.wantSubstr)
			}
			if !strings.Contains(got, "tg://user?id="+itoa(c.userID)) {
				t.Fatalf("expertLine: missing clickable tg://user link for id %d in %q", c.userID, got)
			}
		})
	}
}

func TestMemberUsername(t *testing.T) {
	cases := []struct {
		name string
		cm   *models.ChatMember
		want string
	}{
		{"nil chat member", nil, ""},
		{"member ptr-user", &models.ChatMember{Type: models.ChatMemberTypeMember,
			Member: &models.ChatMemberMember{User: &models.User{Username: "andrey_h"}}}, "andrey_h"},
		{"administrator value-user", &models.ChatMember{Type: models.ChatMemberTypeAdministrator,
			Administrator: &models.ChatMemberAdministrator{User: models.User{Username: "admin_h"}}}, "admin_h"},
		{"owner ptr-user", &models.ChatMember{Type: models.ChatMemberTypeOwner,
			Owner: &models.ChatMemberOwner{User: &models.User{Username: "owner_h"}}}, "owner_h"},
		{"left without handle", &models.ChatMember{Type: models.ChatMemberTypeLeft,
			Left: &models.ChatMemberLeft{User: &models.User{Username: ""}}}, ""},
		{"member nil user", &models.ChatMember{Type: models.ChatMemberTypeMember,
			Member: &models.ChatMemberMember{User: nil}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := memberUsername(c.cm); got != c.want {
				t.Fatalf("memberUsername(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// itoa — без импорта strconv ради одной проверки в тесте.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
