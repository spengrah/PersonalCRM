package telegram

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/rs/zerolog/log"
)

const (
	backfillBatchSize   = 100
	backfillCursorEvery = 100 // persist cursor every N messages
	backfillChatSleepMs = 200 // sleep between chat history fetches
)

// Backfiller handles initial message history fetching.
type Backfiller struct {
	api             *tg.Client
	messageRepo     *repository.TelegramMessageRepository
	chatConfigRepo  *repository.TelegramChatConfigRepository
	syncRepo        *repository.SyncRepository
	syncStateID     *uuid.UUID
	selfUserID      int64
	groupMaxMembers int
	backfillSince   string // "YYYY-MM-DD"
	onProgress      func(total, completed int)
}

// NewBackfiller creates a new backfiller.
func NewBackfiller(
	api *tg.Client,
	messageRepo *repository.TelegramMessageRepository,
	chatConfigRepo *repository.TelegramChatConfigRepository,
	syncRepo *repository.SyncRepository,
	syncStateID *uuid.UUID,
	selfUserID int64,
	groupMaxMembers int,
	backfillSince string,
	onProgress func(total, completed int),
) *Backfiller {
	return &Backfiller{
		api:             api,
		messageRepo:     messageRepo,
		chatConfigRepo:  chatConfigRepo,
		syncRepo:        syncRepo,
		syncStateID:     syncStateID,
		selfUserID:      selfUserID,
		groupMaxMembers: groupMaxMembers,
		backfillSince:   backfillSince,
		onProgress:      onProgress,
	}
}

// Run executes the full backfill: discover dialogs, then fetch history per chat.
func (b *Backfiller) Run(ctx context.Context) error {
	sinceTime, err := time.Parse("2006-01-02", b.backfillSince)
	if err != nil {
		return fmt.Errorf("parse backfill_since %q: %w", b.backfillSince, err)
	}
	sinceUnix := int(sinceTime.Unix())

	log.Info().Str("since", b.backfillSince).Msg("telegram: starting backfill")

	// Phase 1: Discover dialogs
	if err := b.discoverDialogs(ctx); err != nil {
		return fmt.Errorf("discover dialogs: %w", err)
	}

	// Phase 2: Fetch history for chats needing backfill
	chats, err := b.chatConfigRepo.ListForBackfill(ctx)
	if err != nil {
		return fmt.Errorf("list for backfill: %w", err)
	}

	total := len(chats)
	if b.onProgress != nil {
		b.onProgress(total, 0)
	}

	completed := 0
	for _, chat := range chats {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Check if chat should be tracked
		if !EffectiveTracked(chat.Status, chat.MemberCount, b.groupMaxMembers) {
			// Mark as complete immediately (skipped)
			if err := b.chatConfigRepo.UpdateBackfillComplete(ctx, chat.TelegramChatID); err != nil {
				log.Warn().Err(err).Int64("chat_id", chat.TelegramChatID).Msg("telegram: failed to mark skipped chat complete")
			}
			completed++
			if b.onProgress != nil {
				b.onProgress(total, completed)
			}
			continue
		}

		if err := b.backfillChat(ctx, chat, sinceUnix); err != nil {
			log.Warn().Err(err).Int64("chat_id", chat.TelegramChatID).Msg("telegram: backfill failed for chat, skipping")
		}

		completed++
		if b.onProgress != nil {
			b.onProgress(total, completed)
		}

		// Rate limit between chats
		time.Sleep(time.Duration(backfillChatSleepMs) * time.Millisecond)
	}

	log.Info().Int("total_chats", total).Msg("telegram: backfill complete")
	return nil
}

// BackfillChat backfills a single chat by ID (used for retroactive backfill).
func (b *Backfiller) BackfillChat(ctx context.Context, chatID int64) error {
	sinceTime, err := time.Parse("2006-01-02", b.backfillSince)
	if err != nil {
		return fmt.Errorf("parse backfill_since: %w", err)
	}

	cfg, err := b.chatConfigRepo.GetConfig(ctx, chatID)
	if err != nil {
		return fmt.Errorf("get chat config: %w", err)
	}

	return b.backfillChat(ctx, *cfg, int(sinceTime.Unix()))
}

func (b *Backfiller) discoverDialogs(ctx context.Context) error {
	q := query.NewQuery(b.api)
	iter := q.GetDialogs().BatchSize(backfillBatchSize).Iter()

	for iter.Next(ctx) {
		elem := iter.Value()

		switch peer := elem.Peer.(type) {
		case *tg.InputPeerUser:
			// Private chat
			if peer.UserID == b.selfUserID {
				continue // skip Saved Messages
			}

			// Check if it's a bot
			if users := elem.Entities.Users(); users != nil {
				if u, ok := users[peer.UserID]; ok && u.Bot {
					continue
				}
			}

			_, err := b.chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
				TelegramChatID: peer.UserID,
				ChatType:       "private",
				Status:         "auto",
			})
			if err != nil {
				log.Warn().Err(err).Int64("user_id", peer.UserID).Msg("telegram: failed to upsert private chat config")
			}

		case *tg.InputPeerChat:
			// Group chat
			var memberCount *int32
			var title *string
			if chats := elem.Entities.Chats(); chats != nil {
				if c, ok := chats[peer.ChatID]; ok {
					t := c.Title
					title = &t
					mc := int32(c.ParticipantsCount)
					memberCount = &mc
				}
			}

			_, err := b.chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
				TelegramChatID: peer.ChatID,
				ChatTitle:      title,
				ChatType:       "group",
				MemberCount:    memberCount,
				Status:         "auto",
			})
			if err != nil {
				log.Warn().Err(err).Int64("chat_id", peer.ChatID).Msg("telegram: failed to upsert group chat config")
			}

		case *tg.InputPeerChannel:
			// Supergroups and channels — skip per spec
			continue
		}
	}

	if err := iter.Err(); err != nil {
		if ok, waitErr := tgerr.FloodWait(ctx, err); ok {
			if waitErr != nil {
				return fmt.Errorf("flood wait during dialog discovery: %w", waitErr)
			}
			// Retry after flood wait — but for simplicity just return the error
			// and let the caller retry on next connect
		}
		return fmt.Errorf("iterate dialogs: %w", err)
	}

	return nil
}

func (b *Backfiller) backfillChat(ctx context.Context, chat repository.TelegramChatConfig, sinceUnix int) error {
	log.Info().
		Int64("chat_id", chat.TelegramChatID).
		Str("chat_type", chat.ChatType).
		Bool("has_cursor", chat.BackfillCursor != nil).
		Msg("telegram: backfilling chat")

	// Build the appropriate input peer
	var inputPeer tg.InputPeerClass
	switch chat.ChatType {
	case "private":
		inputPeer = &tg.InputPeerUser{UserID: chat.TelegramChatID}
	case "group":
		inputPeer = &tg.InputPeerChat{ChatID: chat.TelegramChatID}
	default:
		return fmt.Errorf("unsupported chat type: %s", chat.ChatType)
	}

	// Build history iterator
	q := query.NewQuery(b.api)
	historyBuilder := q.Messages().GetHistory(inputPeer).BatchSize(backfillBatchSize)

	if chat.BackfillCursor != nil {
		// Resume from cursor (iterate backwards from this message ID)
		historyBuilder = historyBuilder.OffsetID(int(*chat.BackfillCursor))
	}
	// Fresh backfill: no offset — start from newest, stop at sinceUnix in the loop

	iter := historyBuilder.Iter()
	messageCount := 0
	cursorCount := 0

	for iter.Next(ctx) {
		elem := iter.Value()

		msg, ok := elem.Msg.(*tg.Message)
		if !ok {
			continue // skip MessageService
		}

		// Stop if message is older than backfill horizon
		if msg.Date < sinceUnix {
			break
		}

		parsed := ParseMessage(msg, tg.Entities{
			Users: elem.Entities.Users(),
			Chats: elem.Entities.Chats(),
		}, b.selfUserID)
		if parsed == nil {
			continue
		}

		if _, err := b.messageRepo.UpsertMessage(ctx, parsedToUpsertParams(parsed)); err != nil {
			log.Warn().Err(err).
				Int64("chat_id", chat.TelegramChatID).
				Int32("msg_id", parsed.TelegramMessageID).
				Msg("telegram: failed to upsert backfill message")
			continue
		}
		messageCount++
		cursorCount++

		// Persist cursor periodically
		if cursorCount >= backfillCursorEvery {
			if err := b.chatConfigRepo.UpdateBackfillCursor(ctx, chat.TelegramChatID, parsed.TelegramMessageID); err != nil {
				log.Warn().Err(err).Msg("telegram: failed to update backfill cursor")
			}
			cursorCount = 0
		}
	}

	if err := iter.Err(); err != nil {
		if d, ok := tgerr.AsFloodWait(err); ok {
			log.Warn().Dur("wait", d).Int64("chat_id", chat.TelegramChatID).Msg("telegram: flood wait during backfill")
			time.Sleep(d + time.Second)
		}
		return fmt.Errorf("iterate history for chat %d: %w", chat.TelegramChatID, err)
	}

	// Mark backfill complete
	if err := b.chatConfigRepo.UpdateBackfillComplete(ctx, chat.TelegramChatID); err != nil {
		return fmt.Errorf("mark backfill complete: %w", err)
	}

	// Update sync timestamp
	if b.syncStateID != nil {
		_, _ = b.syncRepo.UpdateSyncStateStatus(ctx, *b.syncStateID, repository.SyncStatusIdle, nil)
	}

	log.Info().
		Int64("chat_id", chat.TelegramChatID).
		Int("messages", messageCount).
		Msg("telegram: chat backfill complete")

	return nil
}
