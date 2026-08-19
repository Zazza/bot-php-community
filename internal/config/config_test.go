package config

import "testing"

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PHPBOT_TG_TOKEN", "test-token")
	t.Setenv("PHPBOT_DB_URL", "postgres://test")
	t.Setenv("PHPBOT_LLM_API_KEY", "test-key")
	t.Setenv("PHPBOT_CHAT_ID", "-100200300")
}

// TestClampSpamEscalation — clamp в Load: нулевые/отрицательные пороги эскалации не
// отключают её (Escalate* → минимум 1), цензы (Trust/Voter/NewbieMsgs) не уходят
// в минус (→ минимум 0).
func TestClampSpamEscalation(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		escSpam    int
		escOk      int
		trustMsgs  int
		voterMsgs  int
		newbieMsgs int
	}{
		{"defaults", nil, 3, 2, 30, 10, 8},
		{"escalate_spam_zero", map[string]string{"PHPBOT_SPAM_ESCALATE_SPAM": "0"}, 1, 2, 30, 10, 8},
		{"escalate_spam_negative", map[string]string{"PHPBOT_SPAM_ESCALATE_SPAM": "-7"}, 1, 2, 30, 10, 8},
		{"escalate_ok_zero", map[string]string{"PHPBOT_SPAM_ESCALATE_OK": "0"}, 3, 1, 30, 10, 8},
		{"escalate_ok_negative", map[string]string{"PHPBOT_SPAM_ESCALATE_OK": "-2"}, 3, 1, 30, 10, 8},
		{"trust_negative", map[string]string{"PHPBOT_SPAM_TRUST_MSGS": "-5"}, 3, 2, 0, 10, 8},
		{"voter_negative", map[string]string{"PHPBOT_SPAM_VOTER_MSGS": "-3"}, 3, 2, 30, 0, 8},
		{"newbie_negative", map[string]string{"PHPBOT_SPAM_NEWBIE_MSGS": "-1"}, 3, 2, 30, 10, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setRequiredEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.SpamEscalateSpam != c.escSpam {
				t.Errorf("SpamEscalateSpam = %d, want %d", cfg.SpamEscalateSpam, c.escSpam)
			}
			if cfg.SpamEscalateOk != c.escOk {
				t.Errorf("SpamEscalateOk = %d, want %d", cfg.SpamEscalateOk, c.escOk)
			}
			if cfg.SpamTrustMsgs != c.trustMsgs {
				t.Errorf("SpamTrustMsgs = %d, want %d", cfg.SpamTrustMsgs, c.trustMsgs)
			}
			if cfg.SpamVoterMsgs != c.voterMsgs {
				t.Errorf("SpamVoterMsgs = %d, want %d", cfg.SpamVoterMsgs, c.voterMsgs)
			}
			if cfg.SpamNewbieMsgs != c.newbieMsgs {
				t.Errorf("SpamNewbieMsgs = %d, want %d", cfg.SpamNewbieMsgs, c.newbieMsgs)
			}
		})
	}
}
