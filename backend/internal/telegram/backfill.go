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

// dialogInfo holds a discovered dialog's peer and config for backfill.
type dialogInfo struct {
	peer   tg.InputPeerClass
	config repository.TelegramChatConfig
}

// Run executes the full backfill: discover dialogs and fetch history per chat.
func (b *Backfiller) Run(ctx context.Context) error {
	sinceTime, err := time.Parse("2006-01-02", b.backfillSince)
	if err != nil {
		return fmt.Errorf("parse backfill_since %q: %w", b.backfillSince, err)
	}
	sinceUnix := int(sinceTime.Unix())

	log.Info().Str("since", b.backfillSince).Msg("telegram: starting backfill")

	// Discover dialogs and collect peers (with access hashes) for backfill
	dialogs, err := b.discoverDialogs(ctx, sinceUnix)
	if err != nil {
		return fmt.Errorf("discover dialogs: %w", err)
	}

	total := len(dialogs)
	if b.onProgress != nil {
		b.onProgress(total, 0)
	}

	completed := 0
	for _, d := range dialogs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Skip already-completed chats
		if d.config.BackfillComplete {
			completed++
			if b.onProgress != nil {
				b.onProgress(total, completed)
			}
			continue
		}

		// Check if chat should be tracked
		if !EffectiveTracked(d.config.Status, d.config.MemberCount, b.groupMaxMembers) {
			if err := b.chatConfigRepo.UpdateBackfillComplete(ctx, d.config.TelegramChatID); err != nil {
				log.Warn().Err(err).Int64("chat_id", d.config.TelegramChatID).Msg("telegram: failed to mark skipped chat complete")
			}
			completed++
			if b.onProgress != nil {
				b.onProgress(total, completed)
			}
			continue
		}

		if err := b.backfillChatWithPeer(ctx, d.config, d.peer, sinceUnix); err != nil {
			log.Warn().Err(err).Int64("chat_id", d.config.TelegramChatID).Msg("telegram: backfill failed for chat, skipping")
		}

		completed++
		if b.onProgress != nil {
			b.onProgress(total, completed)
		}

		time.Sleep(time.Duration(backfillChatSleepMs) * time.Millisecond)
	}

	log.Info().Int("total_chats", total).Msg("telegram: backfill complete")
	return nil
}

// BackfillChat backfills a single chat by ID (used for retroactive backfill).
// Access hashes are not available for retroactive backfill; private chat
// history may fail if the server requires one.
func (b *Backfiller) BackfillChat(ctx context.Context, chatID int64) error {
	sinceTime, err := time.Parse("2006-01-02", b.backfillSince)
	if err != nil {
		return fmt.Errorf("parse backfill_since: %w", err)
	}

	cfg, err := b.chatConfigRepo.GetConfig(ctx, chatID)
	if err != nil {
		return fmt.Errorf("get chat config: %w", err)
	}

	// Build peer — for retroactive backfill we don't have the access hash cached,
	// so use InputPeerChat for groups (doesn't need hash) and InputPeerUser for
	// private chats (may fail without hash — acceptable for retroactive backfill)
	var peer tg.InputPeerClass
	switch cfg.ChatType {
	case "private":
		peer = &tg.InputPeerUser{UserID: cfg.TelegramChatID}
	case "group":
		peer = &tg.InputPeerChat{ChatID: cfg.TelegramChatID}
	default:
		return fmt.Errorf("unsupported chat type: %s", cfg.ChatType)
	}

	if b.onProgress != nil {
		b.onProgress(1, 0)
	}
	err = b.backfillChatWithPeer(ctx, *cfg, peer, int(sinceTime.Unix()))
	if b.onProgress != nil {
		b.onProgress(1, 1)
	}
	return err
}

func (b *Backfiller) discoverDialogs(ctx context.Context, sinceUnix int) ([]dialogInfo, error) {
	q := query.NewQuery(b.api)
	iter := q.GetDialogs().BatchSize(backfillBatchSize).Iter()

	var dialogs []dialogInfo

	for iter.Next(ctx) {
		elem := iter.Value()

		// Skip dialogs with no recent activity
		if elem.Last != nil && elem.Last.GetDate() < sinceUnix {
			continue
		}

		switch peer := elem.Peer.(type) {
		case *tg.InputPeerUser:
			if peer.UserID == b.selfUserID {
				continue
			}
			if users := elem.Entities.Users(); users != nil {
				if u, ok := users[peer.UserID]; ok && u.Bot {
					continue
				}
			}

			cfg, err := b.chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
				TelegramChatID: peer.UserID,
				ChatType:       "private",
				Status:         "auto",
			})
			if err != nil {
				log.Warn().Err(err).Int64("user_id", peer.UserID).Msg("telegram: failed to upsert private chat config")
				continue
			}
			dialogs = append(dialogs, dialogInfo{peer: peer, config: *cfg})

		case *tg.InputPeerChat:
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

			cfg, err := b.chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
				TelegramChatID: peer.ChatID,
				ChatTitle:      title,
				ChatType:       "group",
				MemberCount:    memberCount,
				Status:         "auto",
			})
			if err != nil {
				log.Warn().Err(err).Int64("chat_id", peer.ChatID).Msg("telegram: failed to upsert group chat config")
				continue
			}
			dialogs = append(dialogs, dialogInfo{peer: peer, config: *cfg})

		case *tg.InputPeerChannel:
			continue
		}
	}

	if err := iter.Err(); err != nil {
		if ok, waitErr := tgerr.FloodWait(ctx, err); ok {
			if waitErr != nil {
				return dialogs, fmt.Errorf("flood wait during dialog discovery: %w", waitErr)
			}
			// Wait succeeded — return partial dialogs
			log.Warn().Int("dialogs", len(dialogs)).Msg("telegram: flood wait during dialog discovery, returning partial results")
			return dialogs, nil
		}
		return dialogs, fmt.Errorf("iterate dialogs: %w", err)
	}

	log.Info().Int("dialogs", len(dialogs)).Msg("telegram: dialog discovery complete")
	return dialogs, nil
}

func (b *Backfiller) backfillChatWithPeer(ctx context.Context, chat repository.TelegramChatConfig, peer tg.InputPeerClass, sinceUnix int) error {
	log.Info().
		Int64("chat_id", chat.TelegramChatID).
		Str("chat_type", chat.ChatType).
		Bool("has_cursor", chat.BackfillCursor != nil).
		Msg("telegram: backfilling chat")

	q := query.NewQuery(b.api)
	historyBuilder := q.Messages().GetHistory(peer).BatchSize(backfillBatchSize)

	if chat.BackfillCursor != nil {
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
			continue
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

	if err := b.chatConfigRepo.UpdateBackfillComplete(ctx, chat.TelegramChatID); err != nil {
		return fmt.Errorf("mark backfill complete: %w", err)
	}

	if b.syncStateID != nil {
		_, _ = b.syncRepo.UpdateSyncStateStatus(ctx, *b.syncStateID, repository.SyncStatusIdle, nil)
	}

	log.Info().
		Int64("chat_id", chat.TelegramChatID).
		Int("messages", messageCount).
		Msg("telegram: chat backfill complete")

	return nil
}
