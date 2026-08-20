package moderation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// ErrVoteAlreadyActive — голосование за этого пользователя уже открыто.
var ErrVoteAlreadyActive = errors.New("vote already active")

// ErrReportCooldown — инициатор жалуется слишком часто.
var ErrReportCooldown = errors.New("report cooldown")

// reportCooldown — минимальный интервал между жалобами одного инициатора.
const reportCooldown = 5 * time.Minute

// maxReasonLen — ограничение длины причины в посте (анти-амплификация чужого текста).
const maxReasonLen = 200

type VoteConfig struct {
	Window time.Duration
	Quorum int
}

type VoteToKick struct {
	api       *bot.Bot
	repo      *Repository
	adminIDs  map[int64]struct{}
	botUserID int64
	cfg       VoteConfig

	mu         sync.Mutex
	lastReport map[int64]time.Time

	stop chan struct{}
	wg   sync.WaitGroup
}

func NewVoteToKick(api *bot.Bot, repo *Repository, adminIDs []int64, botUserID int64, cfg VoteConfig) *VoteToKick {
	amap := make(map[int64]struct{}, len(adminIDs))
	for _, id := range adminIDs {
		amap[id] = struct{}{}
	}
	return &VoteToKick{
		api:        api,
		repo:       repo,
		adminIDs:   amap,
		botUserID:  botUserID,
		cfg:        cfg,
		lastReport: make(map[int64]time.Time),
		stop:       make(chan struct{}),
	}
}

func (v *VoteToKick) isAdmin(userID int64) bool {
	_, ok := v.adminIDs[userID]
	return ok
}

func truncateReason(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxReasonLen {
		return s
	}
	return string(r[:maxReasonLen]) + "…"
}

// StartVote создаёт голосование и постит сообщение с кнопками.
// Одно активное голосование на цель; per-инициатор cooldown; бота изгнать нельзя.
// targetName — first_name [+ last_name] цели: без @username пост не анонимен.
func (v *VoteToKick) StartVote(ctx context.Context, chatID, targetUserID int64, targetUsername, targetName, reason string, reporterID int64) error {
	if targetUserID == v.botUserID {
		return fmt.Errorf("бота изгнать нельзя")
	}

	v.mu.Lock()
	if last, ok := v.lastReport[reporterID]; ok && time.Since(last) < reportCooldown {
		remaining := reportCooldown - time.Since(last)
		v.mu.Unlock()
		return fmt.Errorf("слишком частые жалобы — подождите %v: %w", remaining.Round(time.Second), ErrReportCooldown)
	}
	v.lastReport[reporterID] = time.Now()
	v.mu.Unlock()

	existing, err := v.repo.ActiveVoteForTarget(ctx, chatID, targetUserID)
	if err != nil {
		return fmt.Errorf("active vote check: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("голосование уже идёт — %s: %w",
			userLabel(targetUsername, targetName, "этот участник"), ErrVoteAlreadyActive)
	}

	reason = truncateReason(sanitizeReason(reason))
	closesAt := time.Now().Add(v.cfg.Window)
	voteID, err := v.repo.CreateVote(ctx, chatID, targetUserID, targetUsername, targetName, reason, closesAt)
	if err != nil {
		return fmt.Errorf("create vote: %w", err)
	}
	text := fmt.Sprintf("🗳 Голосование за изгнание: %s\nПричина: %s\nЗакроется через %v или при кворуме %d голосов «за».",
		userLabel(targetUsername, targetName, "участник"), reason, v.cfg.Window, v.cfg.Quorum)
	sent, err := v.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: voteKeyboard(voteID),
	})
	if err != nil {
		_, _ = v.repo.ResolveVote(ctx, voteID, "closed")
		return fmt.Errorf("send vote message: %w", err)
	}
	if sent != nil {
		_ = v.repo.SetVoteMessage(ctx, voteID, int64(sent.ID))
	}
	return nil
}

// HandleVoteCallback обрабатывает тап по кнопке голоса. Возвращает текст toast.
// cb.Data = "vote:<id>:<for|against|close>".
func (v *VoteToKick) HandleVoteCallback(ctx context.Context, cb *models.CallbackQuery) string {
	if cb.From.ID == 0 {
		return "—"
	}
	parts := strings.Split(cb.Data, ":") // vote:<id>:<choice>
	if len(parts) != 3 {
		return "Некорректная кнопка"
	}
	voteID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "Некорректная кнопка"
	}
	choice := parts[2]

	vr, err := v.repo.GetVote(ctx, voteID)
	if err != nil || vr == nil {
		return "Голосование уже не активно"
	}
	if vr.Outcome != "open" {
		return "Голосование уже закрыто"
	}

	voterID := cb.From.ID
	if voterID == vr.TargetUserID {
		return "Нельзя голосовать за себя"
	}
	if voterID == v.botUserID {
		return "—"
	}

	if choice == "close" {
		if !v.isAdmin(voterID) {
			return "Закрыть может только админ"
		}
		if err := v.CloseAdmin(ctx, voteID); err != nil {
			slog.Warn("vote close admin", "err", err)
			return "Ошибка закрытия"
		}
		return "Голосование закрыто админом"
	}
	if choice != "for" && choice != "against" {
		return "Некорректный выбор"
	}

	forN, againstN, dup, err := v.repo.CastBallot(ctx, voteID, voterID, choice)
	if err != nil {
		slog.Warn("cast ballot", "err", err)
		return "Ошибка голосования"
	}
	if dup {
		return "Ты уже голосовал"
	}
	slog.Info("vote cast", "vote", voteID, "user", voterID, "choice", choice)

	if decideOutcome(forN, againstN, v.cfg.Quorum) == "kicked" {
		v.resolveVote(ctx, voteID)
		return "Кворум достигнут — изгнание!"
	}
	if choice == "for" {
		return fmt.Sprintf("Принято: %d за / %d против (кворум %d)", forN, againstN, v.cfg.Quorum)
	}
	return fmt.Sprintf("Принято: %d за / %d против", forN, againstN)
}

// CloseAdmin закрывает голосование без кика, по решению админа (veto).
func (v *VoteToKick) CloseAdmin(ctx context.Context, voteID int64) error {
	vr, err := v.repo.GetVote(ctx, voteID)
	if err != nil || vr == nil {
		return fmt.Errorf("vote not found")
	}
	if vr.Outcome != "open" {
		return nil
	}
	claimed, err := v.repo.ResolveVote(ctx, voteID, "closed")
	if err != nil {
		return fmt.Errorf("resolve vote: %w", err)
	}
	if !claimed {
		return nil
	}
	_, _ = v.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: vr.ChatID,
		Text: fmt.Sprintf("🗳 Голосование закрыто админом: %s (%d за / %d против).",
			userLabel(vr.TargetUsername, vr.TargetName, "участник"), vr.ForCount, vr.AgainstCount),
	})
	v.editVoteMessage(ctx, vr, "closed")
	return nil
}

func (v *VoteToKick) Start(ctx context.Context) {
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-v.stop:
				return
			case <-t.C:
				v.sweep(ctx)
			}
		}
	}()
	slog.Info("vote sweeper started")
}

func (v *VoteToKick) Stop() {
	close(v.stop)
	v.wg.Wait()
}

func (v *VoteToKick) sweep(ctx context.Context) {
	now := time.Now()
	due, err := v.repo.VotesDue(ctx, now)
	if err != nil {
		slog.Warn("votes due", "err", err)
		return
	}
	for i := range due {
		v.resolveVote(ctx, due[i].ID)
	}
}

// resolveVote подводит итог по СВЕЖИМ счётчикам: кикает (reversible) при кворуме или
// закрывает. Переход open→final атомарен (ResolveVote с WHERE outcome='open' и проверкой
// RowsAffected), поэтому side-effect (кик/пост/правка) выполняется ровно один раз — даже
// при гонке callback против свипера или двух решающих голосов одновременно.
func (v *VoteToKick) resolveVote(ctx context.Context, voteID int64) {
	vr, err := v.repo.GetVote(ctx, voteID)
	if err != nil || vr == nil {
		return
	}
	if vr.Outcome != "open" {
		return
	}
	outcome := decideOutcome(vr.ForCount, vr.AgainstCount, v.cfg.Quorum)
	claimed, err := v.repo.ResolveVote(ctx, voteID, outcome)
	if err != nil {
		slog.Warn("resolve vote", "err", err)
		return
	}
	if !claimed {
		return
	}
	if outcome == "kicked" {
		if err := kickUserReversible(ctx, v.api, vr.ChatID, vr.TargetUserID); err != nil {
			slog.Warn("vote kick reversible", "err", err)
			_, _ = v.api.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: vr.ChatID,
				Text: fmt.Sprintf("⚠️ Кик по голосованию не удался — попросите админа /kick: %s.",
					userLabel(vr.TargetUsername, vr.TargetName, "участник")),
			})
		} else {
			_, _ = v.api.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: vr.ChatID,
				Text: fmt.Sprintf("⛔ Изгнание по итогам голосования: %s (%d за / %d против).",
					userLabel(vr.TargetUsername, vr.TargetName, "участник"), vr.ForCount, vr.AgainstCount),
			})
			slog.Info("vote kicked", "target", vr.TargetUserID, "for", vr.ForCount, "against", vr.AgainstCount)
		}
	} else {
		_, _ = v.api.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: vr.ChatID,
			Text: fmt.Sprintf("🗳 Голосование закрыто: кворум не достигнут (%d за / %d против).",
				vr.ForCount, vr.AgainstCount),
		})
	}
	v.editVoteMessage(ctx, vr, outcome)
}

func (v *VoteToKick) editVoteMessage(ctx context.Context, vr *VoteRecord, outcome string) {
	if vr.MessageID == 0 {
		return
	}
	var summary string
	if outcome == "kicked" {
		summary = fmt.Sprintf("Исход: изгнание (%d за / %d против)", vr.ForCount, vr.AgainstCount)
	} else {
		summary = fmt.Sprintf("Исход: кворум не достигнут (%d за / %d против)", vr.ForCount, vr.AgainstCount)
	}
	newText := fmt.Sprintf("🗳 Голосование завершено: %s.\n%s",
		userLabel(vr.TargetUsername, vr.TargetName, "участник"), summary)
	_, err := v.api.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:      vr.ChatID,
		MessageID:   int(vr.MessageID),
		Text:        newText,
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}},
	})
	if err != nil {
		slog.Warn("edit vote message", "err", err)
	}
}

// decideOutcome — "kicked" если «за» >= кворума и строго больше «против», иначе "closed".
func decideOutcome(forCount, againstCount, quorum int) string {
	if forCount >= quorum && forCount > againstCount {
		return "kicked"
	}
	return "closed"
}

// voteKeyboard — кнопки голосования. callback_data: vote:<id>:<for|against|close>.
func voteKeyboard(voteID int64) models.InlineKeyboardMarkup {
	prefix := "vote:" + strconv.FormatInt(voteID, 10) + ":"
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{
			{Text: "👍 За изгнание", CallbackData: prefix + "for"},
			{Text: "👎 Против", CallbackData: prefix + "against"},
		},
		{
			{Text: "🔒 Закрыть (админ)", CallbackData: prefix + "close"},
		},
	}}
}
