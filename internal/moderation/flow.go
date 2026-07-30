package moderation

import (
	"context"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"phpbot/internal/llm"
	"phpbot/internal/messages"
	"phpbot/internal/users"
)

// Flow оркестрирует гейт новичков: мьют + капча (математика, inline-кнопки) и свипер
// (кик по таймауту, cleanup ушедших, снятие линк-провации). Judge/ManualCheck
// остаются для ручной /check-проверки существующих участников.
type Flow struct {
	api      *bot.Bot
	llm      *llm.LLMClient
	repo     *Repository
	users    *users.Repository
	msgs     *messages.Repository
	adminIDs map[int64]struct{}

	gateEnabled bool
	captchaTO   time.Duration
	maxAttempts int
	probation   time.Duration

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewFlow создаёт Flow.
func NewFlow(api *bot.Bot, llm *llm.LLMClient, repo *Repository, usersRepo *users.Repository, msgsRepo *messages.Repository,
	gateEnabled bool, captchaTimeout time.Duration, maxAttempts int, probation time.Duration,
	adminIDs []int64) *Flow {
	amap := make(map[int64]struct{}, len(adminIDs))
	for _, id := range adminIDs {
		amap[id] = struct{}{}
	}
	return &Flow{
		api:         api,
		llm:         llm,
		repo:        repo,
		users:       usersRepo,
		msgs:        msgsRepo,
		adminIDs:    amap,
		gateEnabled: gateEnabled,
		captchaTO:   captchaTimeout,
		maxAttempts: maxAttempts,
		probation:   probation,
		stop:        make(chan struct{}),
	}
}

// ManualCheck — ручная judge-проверка пользователя (команда /check @user).
func (f *Flow) ManualCheck(ctx context.Context, chatID, userID int64, username, text string) Verdict {
	v := Judge(ctx, f.llm, text)
	_, _ = f.repo.SaveVerdict(ctx, &VerdictRecord{
		TGUserID: userID, ChatID: chatID,
		Question: "(manual /check)", Answer: text,
		Verdict: v.Verdict, Reason: v.Reason,
	})
	return v
}

// IsAdmin проверяет, является ли userID админом.
func (f *Flow) IsAdmin(userID int64) bool {
	_, ok := f.adminIDs[userID]
	return ok
}

func (f *Flow) kickUser(ctx context.Context, chatID, userID int64) error {
	return kickUserReversible(ctx, f.api, chatID, userID)
}

// KickReversible — обратимый кик (бан с UntilDate + unban only-if-banned).
// Используется командой /kick; повторяет путь модерации.
func (f *Flow) KickReversible(ctx context.Context, chatID, userID int64) error {
	return kickUserReversible(ctx, f.api, chatID, userID)
}
