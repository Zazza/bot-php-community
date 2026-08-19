package moderation

import (
	"strconv"
	"testing"
	"time"
)

// TestParseEscalationCallback — spamesc:<flagID>:<verb>. Мусор → ok=false:
// кнопка не обрабатывается (toast «Некорректная кнопка»), fail-safe.
func TestParseEscalationCallback(t *testing.T) {
	cases := []struct {
		name     string
		data     string
		wantID   int64
		wantVerb string
		wantOK   bool
	}{
		{"spam", "spamesc:123:spam", 123, "spam", true},
		{"ok", "spamesc:123:ok", 123, "ok", true},
		{"ban", "spamesc:7:ban", 7, "ban", true},
		{"restore", "spamesc:7:restore", 7, "restore", true},
		{"wrong_prefix", "vote:1:for", 0, "", false},
		{"empty_data", "", 0, "", false},
		{"missing_parts", "spamesc:", 0, "", false},
		{"non_numeric_id", "spamesc:abc:spam", 0, "", false},
		{"extra_parts", "spamesc:1:2:3", 0, "", false},
		{"unknown_verb", "spamesc:1:unknown", 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotID, gotVerb, gotOK := parseEscalationCallback(c.data)
			if gotID != c.wantID || gotVerb != c.wantVerb || gotOK != c.wantOK {
				t.Errorf("parseEscalationCallback(%q) = (%d, %q, %v), want (%d, %q, %v)",
					c.data, gotID, gotVerb, gotOK, c.wantID, c.wantVerb, c.wantOK)
			}
		})
	}
}

// TestDecideEscalation — "escalate" при spamN>=escSpam, "clear" при okN>=escOk;
// оба порога → "escalate" (приоритет более строгому исходу, зеркало decideOutcome).
func TestDecideEscalation(t *testing.T) {
	cases := []struct {
		name    string
		spamN   int
		okN     int
		escSpam int
		escOk   int
		want    string
	}{
		{"below_both", 1, 1, 3, 2, ""},
		{"spam_at_threshold", 3, 0, 3, 2, "escalate"},
		{"spam_above_threshold", 5, 0, 3, 2, "escalate"},
		{"ok_at_threshold", 0, 2, 3, 2, "clear"},
		{"ok_above_threshold", 2, 4, 3, 2, "clear"},
		{"both_escalate_priority", 3, 2, 3, 2, "escalate"},
		{"thresholds_one_one_spam", 1, 0, 1, 1, "escalate"},
		{"thresholds_one_one_ok", 0, 1, 1, 1, "clear"},
		{"thresholds_one_one_both", 1, 1, 1, 1, "escalate"},
		{"zero_counters", 0, 0, 3, 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideEscalation(c.spamN, c.okN, c.escSpam, c.escOk)
			if got != c.want {
				t.Errorf("decideEscalation(spam=%d, ok=%d, escSpam=%d, escOk=%d) = %q, want %q",
					c.spamN, c.okN, c.escSpam, c.escOk, got, c.want)
			}
		})
	}
}

// TestSpamKeyboard — пост-предупреждение: один ряд из 2 кнопок с callback_data
// spamesc:<flagID>:{spam,ok}, flagID корректно сериализован.
func TestSpamKeyboard(t *testing.T) {
	cases := []struct {
		name   string
		flagID int64
	}{
		{"small", 1},
		{"two_digit", 42},
		{"large", 9999999999},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kb := spamKeyboard(c.flagID)
			if len(kb.InlineKeyboard) != 1 {
				t.Fatalf("expected 1 row, got %d", len(kb.InlineKeyboard))
			}
			row := kb.InlineKeyboard[0]
			if len(row) != 2 {
				t.Fatalf("expected 2 buttons in row, got %d", len(row))
			}
			prefix := escCallbackPrefix + strconv.FormatInt(c.flagID, 10) + ":"
			if got := row[0].CallbackData; got != prefix+"spam" {
				t.Errorf("button[0].CallbackData = %q, want %q", got, prefix+"spam")
			}
			if got := row[1].CallbackData; got != prefix+"ok" {
				t.Errorf("button[1].CallbackData = %q, want %q", got, prefix+"ok")
			}
			for _, b := range row {
				if b.Text == "" {
					t.Errorf("empty button text for callback %q", b.CallbackData)
				}
			}
		})
	}
}

// TestAdminKeyboard — ЛС-уведомление админу: один ряд из 2 кнопок с callback_data
// spamesc:<flagID>:{ban,restore}.
func TestAdminKeyboard(t *testing.T) {
	cases := []struct {
		name   string
		flagID int64
	}{
		{"small", 1},
		{"large", 9999999999},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kb := adminKeyboard(c.flagID)
			if len(kb.InlineKeyboard) != 1 {
				t.Fatalf("expected 1 row, got %d", len(kb.InlineKeyboard))
			}
			row := kb.InlineKeyboard[0]
			if len(row) != 2 {
				t.Fatalf("expected 2 buttons in row, got %d", len(row))
			}
			prefix := escCallbackPrefix + strconv.FormatInt(c.flagID, 10) + ":"
			if got := row[0].CallbackData; got != prefix+"ban" {
				t.Errorf("button[0].CallbackData = %q, want %q", got, prefix+"ban")
			}
			if got := row[1].CallbackData; got != prefix+"restore" {
				t.Errorf("button[1].CallbackData = %q, want %q", got, prefix+"restore")
			}
			for _, b := range row {
				if b.Text == "" {
					t.Errorf("empty button text for callback %q", b.CallbackData)
				}
			}
		})
	}
}

// TestVoteCooldownLeft — анти-флуд голосов: zero time (первый голос) и истёкшее
// окно → 0 (можно голосовать), внутри окна → остаток до spamVoteCooldown.
func TestVoteCooldownLeft(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		last time.Time
		want time.Duration
	}{
		{"first_vote_zero_time", time.Time{}, 0},
		{"inside_window", now.Add(-time.Minute), 4 * time.Minute},
		{"exactly_elapsed", now.Add(-spamVoteCooldown), 0},
		{"window_expired", now.Add(-time.Hour), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := voteCooldownLeft(c.last, now)
			if got != c.want {
				t.Errorf("voteCooldownLeft(last=%v, now=%v) = %v, want %v", c.last, now, got, c.want)
			}
		})
	}
}

// TestSpamEscalationIsAdmin — админский ценз: только список adminIDs; бот
// (botUserID) и посторонние — не админы.
func TestSpamEscalationIsAdmin(t *testing.T) {
	e := NewSpamEscalation(nil, nil, nil, []int64{10, 20}, 99, EscalationConfig{})
	cases := []struct {
		name string
		user int64
		want bool
	}{
		{"admin_first", 10, true},
		{"admin_second", 20, true},
		{"bot_not_admin", 99, false},
		{"stranger", 777, false},
		{"zero_user", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := e.isAdmin(c.user); got != c.want {
				t.Errorf("isAdmin(%d) = %v, want %v", c.user, got, c.want)
			}
		})
	}
}

// TestShouldReleaseRestrict — кто может снять TG-рестрикт при снятии предупреждения.
// Голоса сообщества («не спам») НЕ снимают авторестрикт по счётчику WarnMax
// (action='restrict') — только собственную эскалацию; админский клик снимает всё.
func TestShouldReleaseRestrict(t *testing.T) {
	cases := []struct {
		name       string
		action     string
		adminClick bool
		escalated  bool
		want       bool
	}{
		{"authoristrict_community_votes", "restrict", false, false, false},
		{"authoristrict_admin_click", "restrict", true, false, true},
		{"authoristrict_community_after_escalation", "restrict", false, true, true},
		{"authoristrict_admin_after_escalation", "restrict", true, true, true},
		{"escalated_warn_row", "warn", false, true, true},
		{"plain_warn_no_restrict", "warn", false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldReleaseRestrict(c.action, c.adminClick, c.escalated); got != c.want {
				t.Errorf("shouldReleaseRestrict(%q, admin=%v, escalated=%v) = %v, want %v",
					c.action, c.adminClick, c.escalated, got, c.want)
			}
		})
	}
}

// TestBannedAction — распознание состояния «ошибочный бан» для restore-поверх-бана.
func TestBannedAction(t *testing.T) {
	banned, restored, other := "banned", "restored", "kicked"
	cases := []struct {
		name   string
		action *string
		want   bool
	}{
		{"nil", nil, false},
		{"banned", &banned, true},
		{"restored", &restored, false},
		{"other", &other, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bannedAction(c.action); got != c.want {
				t.Errorf("bannedAction(%v) = %v, want %v", c.action, got, c.want)
			}
		})
	}
}
