package moderation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func muteUser(ctx context.Context, api *bot.Bot, chatID, userID int64) error {
	_, err := api.RestrictChatMember(ctx, &bot.RestrictChatMemberParams{
		ChatID:                        chatID,
		UserID:                        userID,
		Permissions:                   &models.ChatPermissions{},
		UseIndependentChatPermissions: true,
	})
	return err
}

func restrictUserTextOnly(ctx context.Context, api *bot.Bot, chatID, userID int64) error {
	_, err := api.RestrictChatMember(ctx, &bot.RestrictChatMemberParams{
		ChatID:                        chatID,
		UserID:                        userID,
		Permissions:                   &models.ChatPermissions{CanSendMessages: true},
		UseIndependentChatPermissions: true,
	})
	return err
}

// restrictUserTextOnlyUntil — как restrictUserTextOnly, но с UntilDate (safety-net на
// уровне TG: ограничение спадёт само, даже если свипер не дойдёт до restrict_until).
func restrictUserTextOnlyUntil(ctx context.Context, api *bot.Bot, chatID, userID int64, until time.Time) error {
	_, err := api.RestrictChatMember(ctx, &bot.RestrictChatMemberParams{
		ChatID:                        chatID,
		UserID:                        userID,
		UntilDate:                     int(until.Unix()),
		Permissions:                   &models.ChatPermissions{CanSendMessages: true},
		UseIndependentChatPermissions: true,
	})
	return err
}

func unmuteUserFull(ctx context.Context, api *bot.Bot, chatID, userID int64) error {
	_, err := api.RestrictChatMember(ctx, &bot.RestrictChatMemberParams{
		ChatID: chatID,
		UserID: userID,
		Permissions: &models.ChatPermissions{
			CanSendMessages: true, CanSendAudios: true, CanSendDocuments: true,
			CanSendPhotos: true, CanSendVideos: true, CanSendVideoNotes: true,
			CanSendVoiceNotes: true, CanSendPolls: true, CanSendOtherMessages: true,
			CanAddWebPagePreviews: true,
		},
		UseIndependentChatPermissions: true,
	})
	return err
}

// kickBanSafetyWindow — до какого момента держать бан, если немедленный unban не прошёл.
// Гарантирует обратимость кика даже при сбое UnbanChatMember (бан сам спадёт к этому сроку).
const kickBanSafetyWindow = 24 * time.Hour

func kickUserReversible(ctx context.Context, api *bot.Bot, chatID, userID int64) error {
	until := time.Now().Add(kickBanSafetyWindow)
	if _, err := api.BanChatMember(ctx, &bot.BanChatMemberParams{
		ChatID: chatID, UserID: userID, UntilDate: int(until.Unix()),
	}); err != nil {
		return fmt.Errorf("ban: %w", err)
	}
	if _, err := api.UnbanChatMember(ctx, &bot.UnbanChatMemberParams{
		ChatID: chatID, UserID: userID, OnlyIfBanned: true,
	}); err != nil {
		slog.Error("unban failed: user may remain banned until safety window",
			"chat_id", chatID, "user_id", userID, "until", until, "err", err)
		return fmt.Errorf("unban: %w", err)
	}
	return nil
}

func deleteChatMessage(ctx context.Context, api *bot.Bot, chatID, messageID int64) {
	if messageID == 0 {
		return
	}
	if _, err := api.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: int(messageID)}); err != nil {
		slog.Warn("delete message", "err", err)
	}
}

func isUserGone(ctx context.Context, api *bot.Bot, chatID, userID int64) bool {
	m, err := api.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chatID, UserID: userID})
	if err != nil {
		return false
	}
	return m.Type == models.ChatMemberTypeLeft || m.Type == models.ChatMemberTypeBanned
}
