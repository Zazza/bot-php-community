// Package config загружает переменные окружения (префикс PHPBOT_).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — все настройки бота.
type Config struct {
	TGToken            string
	AdminIDs           []int64
	ChatIDs            []int64
	DBURL              string
	LLMURL             string
	LLMAPIKey          string
	LLMModel           string
	EmbedModel         string
	EmbedDim           int
	QuietThreshold     int
	TopicCron          string
	NewcomerTimeout    time.Duration
	LogPath            string
	SearXNGURL         string
	SearXNGMax         int
	GateEnabled        bool
	CaptchaTimeout     time.Duration
	CaptchaMaxAttempts int
	Probation          time.Duration
	SpamEnabled        bool
	SpamFloodMsgs      int
	SpamFloodWindow    time.Duration
	SpamWarnMax        int
	SpamWarnPeriod     time.Duration
	SpamRestrictHours  time.Duration
	VoteEnabled        bool
	VoteWindow         time.Duration
	VoteQuorum         int
}

// Load читает env. Необходимые переменные — фаталят при отсутствии.
func Load() (*Config, error) {
	c := &Config{
		TGToken:            os.Getenv("PHPBOT_TG_TOKEN"),
		DBURL:              os.Getenv("PHPBOT_DB_URL"),
		LLMURL:             envOr("PHPBOT_LLM_URL", "https://api.vsellm.ru/v1"),
		LLMAPIKey:          os.Getenv("PHPBOT_LLM_API_KEY"),
		LLMModel:           envOr("PHPBOT_LLM_MODEL", "google/gemini-2.5-flash"),
		EmbedModel:         envOr("PHPBOT_EMBED_MODEL", "text-embedding-3-small"),
		EmbedDim:           envInt("PHPBOT_EMBED_DIM", 1536),
		QuietThreshold:     envInt("PHPBOT_QUIET_THRESHOLD", 20),
		TopicCron:          envOr("PHPBOT_TOPIC_CRON", "0 12,20 * * *"),
		NewcomerTimeout:    envDur("PHPBOT_NEWCOMER_TIMEOUT", 5*time.Minute),
		LogPath:            os.Getenv("PHPBOT_LOG_PATH"),
		SearXNGURL:         envOr("PHPBOT_SEARXNG_URL", ""),
		SearXNGMax:         envInt("PHPBOT_SEARXNG_MAX", 5),
		GateEnabled:        envBool("PHPBOT_GATE_ENABLED", true),
		CaptchaTimeout:     envDur("PHPBOT_CAPTCHA_TIMEOUT", 3*time.Minute),
		CaptchaMaxAttempts: envInt("PHPBOT_CAPTCHA_MAX_ATTEMPTS", 3),
		Probation:          time.Duration(envInt("PHPBOT_PROBATION_HOURS", 6)) * time.Hour,
		SpamEnabled:        envBool("PHPBOT_SPAM_ENABLED", true),
		SpamFloodMsgs:      envInt("PHPBOT_SPAM_FLOOD_MSGS", 5),
		SpamFloodWindow:    envDur("PHPBOT_SPAM_FLOOD_WINDOW", 30*time.Second),
		SpamWarnMax:        envInt("PHPBOT_SPAM_WARN_MAX", 3),
		SpamWarnPeriod:     envDur("PHPBOT_SPAM_WARN_PERIOD", 24*time.Hour),
		SpamRestrictHours:  time.Duration(envInt("PHPBOT_SPAM_RESTRICT_HOURS", 6)) * time.Hour,
		VoteEnabled:        envBool("PHPBOT_VOTE_ENABLED", true),
		VoteWindow:         envDur("PHPBOT_VOTE_WINDOW", 15*time.Minute),
		VoteQuorum:         envInt("PHPBOT_VOTE_QUORUM", 3),
	}
	c.AdminIDs = envInt64List("PHPBOT_ADMIN_IDS")
	c.ChatIDs = envInt64List("PHPBOT_CHAT_ID")

	if c.SpamFloodMsgs < 1 {
		c.SpamFloodMsgs = 1
	}
	if c.SpamWarnMax < 1 {
		c.SpamWarnMax = 1
	}
	if c.VoteQuorum < 1 {
		c.VoteQuorum = 1
	}
	if c.SpamRestrictHours < time.Hour {
		c.SpamRestrictHours = time.Hour
	}

	if c.TGToken == "" {
		return nil, fmt.Errorf("PHPBOT_TG_TOKEN не задан")
	}
	if c.DBURL == "" {
		return nil, fmt.Errorf("PHPBOT_DB_URL не задан")
	}
	if c.LLMAPIKey == "" {
		return nil, fmt.Errorf("PHPBOT_LLM_API_KEY не задан")
	}
	if len(c.ChatIDs) == 0 {
		return nil, fmt.Errorf("PHPBOT_CHAT_ID не задан")
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt64List(key string) []int64 {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}
