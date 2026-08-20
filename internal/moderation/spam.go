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
	FloodMsgs       int
	FloodWindow     time.Duration
	WarnMax         int
	WarnPeriod      time.Duration
	RestrictHours   time.Duration
	NewbieMsgs      int
	TrustMsgs       int
	EscalateEnabled bool
}

type SpamInput struct {
	ChatID    int64
	UserID    int64
	Username  string
	Name      string
	MessageID int64
	Text      string
}

type userBucket struct {
	texts []string
	times []time.Time
	last  string
}

// Причины hard-сигналов эвристики: авторитарны без LLM, но invite у доверенного автора
// уходит на классификатор (анонсы мероприятий со ссылкой — не спам).
const (
	reasonInvite      = "invite/реф-ссылка"
	reasonMassMention = "массовые @упоминания"
)

// messageCounter — число сообщений участника в чате (messages.Repository удовлетворяет).
// Нужен для at-risk-проверки: малая история → полная LLM-классификация сообщения.
type messageCounter interface {
	CountByUser(ctx context.Context, chatID, userID int64, limit int) (int, error)
}

type SpamFilter struct {
	api       *bot.Bot
	llm       *llm.LLMClient
	repo      *Repository
	counter   messageCounter
	adminIDs  map[int64]struct{}
	botUserID int64
	cfg       SpamConfig

	mu     sync.Mutex
	recent map[int64]*userBucket

	stop chan struct{}
	wg   sync.WaitGroup
}

func NewSpamFilter(api *bot.Bot, llmClient *llm.LLMClient, repo *Repository, counter messageCounter, adminIDs []int64, botUserID int64, cfg SpamConfig) *SpamFilter {
	amap := make(map[int64]struct{}, len(adminIDs))
	for _, id := range adminIDs {
		amap[id] = struct{}{}
	}
	return &SpamFilter{
		api:       api,
		llm:       llmClient,
		repo:      repo,
		counter:   counter,
		adminIDs:  amap,
		botUserID: botUserID,
		cfg:       cfg,
		recent:    make(map[int64]*userBucket),
		stop:      make(chan struct{}),
	}
}

// IsAtRisk — нужно ли прогонять сообщение через LLM-классификатор даже без срабатывания
// эвристики. True для малоактивных/новых аккаунтов (число сообщений в чате < NewbieMsgs):
// иначе семантический текст-скам без ссылок/@/CAPS проскакивает мимо жёстких сигналов.
// NewbieMsgs<=0 — kill-switch (at-risk выключен). Fail-safe: ошибка/отсутствие счётчика →
// false (не блокируем — сообщение сохраняется как обычно).
func (f *SpamFilter) IsAtRisk(ctx context.Context, in SpamInput) bool {
	if f.counter == nil || f.cfg.NewbieMsgs <= 0 {
		return false
	}
	n, err := f.counter.CountByUser(ctx, in.ChatID, in.UserID, f.cfg.NewbieMsgs)
	if err != nil {
		slog.Warn("spam at-risk count", "err", err)
		return false
	}
	return n < f.cfg.NewbieMsgs
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
		hit, reason = true, reasonInvite
	}
	if !hit && strings.Count(text, "@") >= 3 {
		hit, reason = true, reasonMassMention
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
// через классификатор. Исключение: invite-ссылка от доверенного автора (TrustMsgs)
// уходит классификатору — анонс митапа со ссылкой на регистрацию не спам.
// Возвращает true, если сообщение признано спамом.
// Fail-safe: ошибка LLM или непарсящийся ответ → false (не спам), кроме доверенного
// invite — enforce с исходной причиной (fail-closed: hard-сигнал не должен проходить
// из-за сбоя или мусора классификатора).
func (f *SpamFilter) ClassifyAndEnforce(ctx context.Context, in SpamInput, reason string) bool {
	hard := isHardReason(reason)
	trusted := hard && isInviteReason(reason) && f.trustedAuthor(ctx, in)
	if hard && !trusted {
		f.enforce(ctx, in, reason)
		return true
	}
	spam, lreason, parsed, err := f.classifyLLM(ctx, in.Text)
	if err != nil || !parsed {
		if trusted {
			slog.Warn("spam classify failed, fail-safe enforce invite", "err", err, "parsed", parsed)
			f.enforce(ctx, in, reason)
			return true
		}
		slog.Warn("spam classify failed, fail-safe not-spam", "err", err, "parsed", parsed)
		return false
	}
	if !spam {
		return false
	}
	if lreason == "" {
		if hard {
			lreason = reason
		} else {
			lreason = "признано спам-классификатором"
		}
	}
	f.enforce(ctx, in, lreason)
	return true
}

// isHardReason — авторитарные сигналы, действующие без LLM (неуязвимы к prompt-инъекции).
func isHardReason(reason string) bool {
	switch reason {
	case reasonInvite, reasonMassMention:
		return true
	}
	return false
}

func isInviteReason(reason string) bool {
	return reason == reasonInvite
}

// trustedAuthor — доверенный автор: достаточно сообщений в истории чата (TrustMsgs).
// 0 = выключено; nil-счётчик или ошибка → false (fail-safe: hard-сигнал авторитарен).
func (f *SpamFilter) trustedAuthor(ctx context.Context, in SpamInput) bool {
	if f.cfg.TrustMsgs <= 0 || f.counter == nil {
		return false
	}
	n, err := f.counter.CountByUser(ctx, in.ChatID, in.UserID, f.cfg.TrustMsgs)
	if err != nil {
		slog.Warn("spam trust count", "err", err)
		return false
	}
	return n >= f.cfg.TrustMsgs
}

func (f *SpamFilter) classifyLLM(ctx context.Context, text string) (bool, string, bool, error) {
	system := prompts.Get(prompts.Spam)
	if system == "" {
		return false, "", false, fmt.Errorf("spam prompt empty")
	}
	// Текст юзера — в role:user, системный промпт статичен: так prompt-инъекция в сообщении
	// не может переписать инструкции классификатора.
	resp, _, _, err := f.llm.Chat(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: text},
	})
	if err != nil {
		return false, "", false, fmt.Errorf("spam llm: %w", err)
	}
	spam, reason, parsed := parseSpamVerdict(resp)
	return spam, reason, parsed, nil
}

func (f *SpamFilter) enforce(ctx context.Context, in SpamInput, reason string) {
	reason = truncateReason(sanitizeReason(reason))
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

	flagID, err := f.repo.CreateSpamFlag(ctx, in.ChatID, in.MessageID, in.UserID, in.Username, in.Name, reason, action, restrictUntil)
	if err != nil {
		slog.Warn("create spam flag", "err", err)
		flagID = 0
	}
	deleteChatMessage(ctx, f.api, in.ChatID, in.MessageID)

	params := &bot.SendMessageParams{ChatID: in.ChatID}
	if flagID != 0 && f.cfg.EscalateEnabled {
		params.ReplyMarkup = spamKeyboard(flagID)
	}
	if action == "restrict" {
		if err := restrictUserTextOnlyUntil(ctx, f.api, in.ChatID, in.UserID, *restrictUntil); err != nil {
			slog.Warn("spam restrict user", "err", err)
		}
		params.Text = fmt.Sprintf("🚫 %s — рестрикт на %d ч: только текст (анти-спам). %s",
			userLabel(in.Username, in.Name, "участник"), int(f.cfg.RestrictHours.Hours()), reason)
	} else {
		params.Text = fmt.Sprintf("⚠️ Похоже на спам. Предупреждение %d/%d. %s — %s",
			prior+1, f.cfg.WarnMax, userLabel(in.Username, in.Name, "участник"), reason)
	}
	sent, err := f.api.SendMessage(ctx, params)
	if err != nil {
		slog.Warn("spam post warn", "err", err)
	} else if flagID != 0 && sent != nil {
		if err := f.repo.SetSpamWarnMessage(ctx, flagID, int64(sent.ID)); err != nil {
			slog.Warn("set spam warn message", "err", err)
		}
	}
	if action == "restrict" {
		slog.Info("spam restricted", "user", in.UserID, "prior", prior)
		return
	}
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

// parseSpamVerdict разбирает JSON-вердикт классификатора. parsed=false при невалидном
// ответе: вызывающий решает, fail-closed (доверенный hard-сигнал) или fail-open (мягкий
// путь). Модель может скопировать few-shot-формат «Ответ: {...}» — вырезаем подстроку
// от первого '{' до последнего '}' перед unmarshal.
func parseSpamVerdict(raw string) (spam bool, reason string, parsed bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}

	var v struct {
		Spam   bool   `json:"spam"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		slog.Warn("spam parse failed", "raw", raw, "err", err)
		return false, "", false
	}
	return v.Spam, v.Reason, true
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
