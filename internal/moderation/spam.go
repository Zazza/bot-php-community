package moderation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"phpbot/internal/llm"
	"phpbot/internal/prompts"
)

type SpamConfig struct {
	FloodMsgs     int
	FloodWindow   time.Duration
	WarnMax       int
	WarnPeriod    time.Duration
	RestrictHours time.Duration
}

type SpamInput struct {
	ChatID    int64
	UserID    int64
	Username  string
	MessageID int64
	Text      string
}

type userBucket struct {
	texts []string
	times []time.Time
	last  string
}

type SpamFilter struct {
	api       *bot.Bot
	llm       *llm.LLMClient
	repo      *Repository
	adminIDs  map[int64]struct{}
	botUserID int64
	cfg       SpamConfig

	mu     sync.Mutex
	recent map[int64]*userBucket

	stop chan struct{}
	wg   sync.WaitGroup
}

func NewSpamFilter(api *bot.Bot, llmClient *llm.LLMClient, repo *Repository, adminIDs []int64, botUserID int64, cfg SpamConfig) *SpamFilter {
	amap := make(map[int64]struct{}, len(adminIDs))
	for _, id := range adminIDs {
		amap[id] = struct{}{}
	}
	return &SpamFilter{
		api:       api,
		llm:       llmClient,
		repo:      repo,
		adminIDs:  amap,
		botUserID: botUserID,
		cfg:       cfg,
		recent:    make(map[int64]*userBucket),
		stop:      make(chan struct{}),
	}
}

func (f *SpamFilter) bucket(userID int64) *userBucket {
	b, ok := f.recent[userID]
	if !ok {
		b = &userBucket{}
		f.recent[userID] = b
	}
	return b
}

func (f *SpamFilter) pruneLocked(b *userBucket, now time.Time) {
	cutoff := now.Add(-f.cfg.FloodWindow)
	idx := 0
	for idx < len(b.times) && b.times[idx].Before(cutoff) {
		idx++
	}
	if idx > 0 {
		b.times = b.times[idx:]
		b.texts = b.texts[idx:]
	}
	if len(b.texts) > 32 {
		b.texts = b.texts[len(b.texts)-32:]
		b.times = b.times[len(b.times)-32:]
	}
}

// Heuristic — синхронная быстрая проверка. Обновляет bucket всегда (для flood-детекта)
// и возвращает true, если сообщение подозрительно и требует LLM-проверки.
func (f *SpamFilter) Heuristic(in SpamInput) (hit bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	b := f.bucket(in.UserID)
	now := time.Now()
	f.pruneLocked(b, now)

	text := in.Text
	low := strings.ToLower(text)

	if containsAny(low, "t.me/+", "t.me/joinchat", "joinchat", "/joinchat") {
		hit, reason = true, "invite/реф-ссылка"
	}
	if !hit && strings.Count(text, "@") >= 3 {
		hit, reason = true, "массовые @упоминания"
	}
	if !hit && utf8.RuneCountInString(text) > 12 && capsRatio(text) > 0.6 {
		hit, reason = true, "CAPS-радио"
	}
	if !hit && b.last != "" && strings.TrimSpace(text) == b.last {
		hit, reason = true, "повтор подряд"
	}
	if !hit {
		cnt := 0
		for _, t := range b.times {
			if now.Sub(t) <= f.cfg.FloodWindow {
				cnt++
			}
		}
		if cnt+1 >= f.cfg.FloodMsgs {
			hit, reason = true, "флуд"
		}
	}

	b.texts = append(b.texts, text)
	b.times = append(b.times, now)
	b.last = strings.TrimSpace(text)
	return hit, reason
}

// ClassifyAndEnforce — действие по подозрению. Авторитарные (hard) сигналы эвристики
// (invite/реф-ссылки, массовые @) срабатывают без LLM; мягкие (CAPS, повтор, флуд) —
// через классификатор. Возвращает true, если сообщение признано спамом.
// Fail-safe: любая ошибка LLM → false (не спам, сообщение сохраняется).
func (f *SpamFilter) ClassifyAndEnforce(ctx context.Context, in SpamInput, reason string) bool {
	if isHardReason(reason) {
		f.enforce(ctx, in, reason)
		return true
	}
	spam, lreason, err := f.classifyLLM(ctx, in.Text)
	if err != nil {
		slog.Warn("spam classify failed, fail-safe not-spam", "err", err)
		return false
	}
	if !spam {
		return false
	}
	if lreason == "" {
		lreason = "признано спам-классификатором"
	}
	f.enforce(ctx, in, lreason)
	return true
}

// isHardReason — авторитарные сигналы, действующие без LLM (неуязвимы к prompt-инъекции).
func isHardReason(reason string) bool {
	switch reason {
	case "invite/реф-ссылка", "массовые @упоминания":
		return true
	}
	return false
}

func (f *SpamFilter) classifyLLM(ctx context.Context, text string) (bool, string, error) {
	system := prompts.Get(prompts.Spam)
	if system == "" {
		return false, "", fmt.Errorf("spam prompt empty")
	}
	// Текст юзера — в role:user, системный промпт статичен: так prompt-инъекция в сообщении
	// не может переписать инструкции классификатора.
	resp, _, _, err := f.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: text},
	})
	if err != nil {
		return false, "", fmt.Errorf("spam llm: %w", err)
	}
	spam, reason := parseSpamVerdict(resp)
	return spam, reason, nil
}

func (f *SpamFilter) enforce(ctx context.Context, in SpamInput, reason string) {
	reason = sanitizeReason(reason)
	now := time.Now()
	prior, err := f.repo.CountSpamByUser(ctx, in.UserID, now.Add(-f.cfg.WarnPeriod))
	if err != nil {
		slog.Warn("spam count by user", "err", err)
		prior = 0
	}

	action := "warn"
	var restrictUntil *time.Time
	if prior+1 >= f.cfg.WarnMax {
		action = "restrict"
		until := now.Add(f.cfg.RestrictHours)
		restrictUntil = &until
	}

	if _, err := f.repo.CreateSpamFlag(ctx, in.ChatID, in.MessageID, in.UserID, in.Username, reason, action, restrictUntil); err != nil {
		slog.Warn("create spam flag", "err", err)
	}
	deleteChatMessage(ctx, f.api, in.ChatID, in.MessageID)

	if action == "restrict" {
		if err := restrictUserTextOnlyUntil(ctx, f.api, in.ChatID, in.UserID, *restrictUntil); err != nil {
			slog.Warn("spam restrict user", "err", err)
		}
		_, _ = f.api.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: in.ChatID,
			Text: fmt.Sprintf("🚫 %s — рестрикт на %d ч: только текст (анти-спам). %s",
				atUser(in.Username, "участник"), int(f.cfg.RestrictHours.Hours()), reason),
		})
		slog.Info("spam restricted", "user", in.UserID, "prior", prior)
		return
	}
	_, _ = f.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: in.ChatID,
		Text: fmt.Sprintf("⚠️ Похоже на спам. Предупреждение %d/%d. %s — %s",
			prior+1, f.cfg.WarnMax, atUser(in.Username, "участник"), reason),
	})
	slog.Info("spam warned", "user", in.UserID, "prior", prior)
}

func (f *SpamFilter) Start(ctx context.Context) {
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-f.stop:
				return
			case <-t.C:
				f.sweep(ctx)
			}
		}
	}()
	slog.Info("spam sweeper started")
}

func (f *SpamFilter) Stop() {
	close(f.stop)
	f.wg.Wait()
}

func (f *SpamFilter) sweep(ctx context.Context) {
	now := time.Now()
	due, err := f.repo.SpamRestrictsDue(ctx, now)
	if err != nil {
		slog.Warn("spam restricts due", "err", err)
		return
	}
	for i := range due {
		sf := due[i]
		if err := unmuteUserFull(ctx, f.api, sf.ChatID, sf.TGUserID); err != nil {
			slog.Warn("spam unmute", "err", err, "user", sf.TGUserID)
			continue
		}
		if err := f.repo.ReleaseSpamRestrict(ctx, sf.ID); err != nil {
			slog.Warn("release spam restrict", "err", err)
		}
		slog.Info("spam restrict released", "user", sf.TGUserID)
	}
}

func parseSpamVerdict(raw string) (bool, string) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var v struct {
		Spam   bool   `json:"spam"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		slog.Warn("spam parse failed, fallback not-spam", "raw", raw, "err", err)
		return false, ""
	}
	return v.Spam, v.Reason
}

// spamLinkRe — invite/реф-паттерны для санитизации причины перед постом в чат.
// Чтобы бот не стал ретранслятором спам-ссылок (reason/lreason могут нести чужой текст).
var spamLinkRe = regexp.MustCompile(`(?i)t\.me/\+|t\.me/joinchat|joinchat`)

func sanitizeReason(s string) string {
	return spamLinkRe.ReplaceAllString(s, "…")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func capsRatio(s string) float64 {
	letters := 0
	upper := 0
	for _, r := range s {
		if unicodeIsLetter(r) {
			letters++
			if r >= 'A' && r <= 'Z' {
				upper++
				continue
			}
			if r >= 'А' && r <= 'Я' || r == 'Ё' {
				upper++
			}
		}
	}
	if letters == 0 {
		return 0
	}
	return float64(upper) / float64(letters)
}

func unicodeIsLetter(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z':
		return true
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'А' && r <= 'я':
		return true
	case r == 'Ё' || r == 'ё':
		return true
	}
	return false
}
