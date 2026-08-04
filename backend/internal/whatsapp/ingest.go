package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"

	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// commsChatStore is the slice of CommsMessageRepository the ingest path writes
// through.
type commsChatStore interface {
	UpsertChatMessage(ctx context.Context, params repository.UpsertChatMessageParams) (*repository.CommsMessage, error)
	SoftDeleteUnmatchedTwin(ctx context.Context, source, externalID string) (int64, error)
	HasMatchedChatMessage(ctx context.Context, source, externalID string) (bool, error)
}

// Ingestor stages projected messages into comms_message. It is the single choke
// point the live handler uses today and the history drainer will reuse, so
// backfill and live ingest cannot diverge.
type Ingestor struct {
	commsRepo commsChatStore
	gate      *ChatGate
	matcher   *PeerMatcher
}

var (
	_ MessageIngestor = (*Ingestor)(nil)
	_ GroupInfoBinder = (*Ingestor)(nil)
)

// NewIngestor builds the live-message ingest path.
func NewIngestor(commsRepo commsChatStore, gate *ChatGate, matcher *PeerMatcher) *Ingestor {
	return &Ingestor{commsRepo: commsRepo, gate: gate, matcher: matcher}
}

// BindGroupInfoSource forwards the seam to the gate. Manager.SetIngestor calls
// it before Start, so the source is bound before any client can connect.
func (i *Ingestor) BindGroupInfoSource(src func() GroupInfoFetcher) {
	if i.gate != nil {
		i.gate.BindGroupInfoSource(src)
	}
}

// PeerMatcher exposes the matcher for the post-import hook, which needs the
// same instance the ingest path uses.
func (i *Ingestor) PeerMatcher() *PeerMatcher {
	return i.matcher
}

// IngestMessage stages one projected message.
//
// Only the staging write's failure is returned — a returned error withholds the
// ack, so it is reserved for outcomes a redelivery can genuinely repair. The
// one deliberate addition is the gate's ErrChatGateUndecided: an undecidable
// group stores nothing AND is not treated as handled.
func (i *Ingestor) IngestMessage(ctx context.Context, msg IngestedMessage) error {
	if msg.ChatType == ChatTypeGroup && i.gate != nil {
		tracked, err := i.gate.ShouldTrack(ctx, msg.ChatJID, msg.ChatType)
		if err != nil {
			// Undecidable: withhold the ack so the next delivery can decide.
			return err
		}
		if !tracked {
			// A real decision: the message is handled, just not stored.
			logger.Debug().Str("chat_jid", msg.ChatJID).Msg("whatsapp: group is not tracked; message not stored")
			return nil
		}
	}

	matched := i.resolveContact(ctx, msg)

	// The matched and unmatched upserts have DISJOINT conflict targets, so both
	// rows can exist for one message. The matched row is the survivor, in both
	// directions.
	if matched == nil && msg.PeerJID != nil {
		already, err := i.commsRepo.HasMatchedChatMessage(ctx, syncSource, msg.MessageID)
		if err != nil {
			// Best-effort: the worst case is a duplicate the next attach
			// reconciles, whereas returning would withhold an ack for content
			// that is about to be stored anyway.
			logger.Warn().Err(err).Str("message_id", msg.MessageID).
				Msg("whatsapp: matched-row probe failed; staging unmatched anyway")
		} else if already {
			// Already stored against a contact. An unmatched insert here would
			// mint the duplicate pair with nothing to tombstone it, and would
			// fire discovery for a peer that already has a contact.
			return nil
		}
	}

	meta, err := buildSourceMetadata(msg)
	if err != nil {
		return fmt.Errorf("whatsapp: build source metadata: %w", err)
	}

	direction := repository.InteractionDirectionInbound
	if msg.IsOutgoing {
		direction = repository.InteractionDirectionOutbound
	}

	if _, err := i.commsRepo.UpsertChatMessage(ctx, repository.UpsertChatMessageParams{
		Source:     syncSource,
		ExternalID: msg.MessageID,
		ThreadID:   msg.ChatJID,
		Body:       msg.Body,
		// Snippet stays nil: nothing on this route reads it, and a truncated
		// copy of the body is a second copy of the same content.
		Snippet:          nil,
		PeerHandle:       msg.PeerJID,
		PeerNormalized:   msg.PeerPhoneE164,
		Direction:        direction,
		SentAt:           msg.SentAt,
		AccountID:        msg.AccountJID,
		SourceMetadata:   meta,
		MatchedContactID: matched,
	}); err != nil {
		return fmt.Errorf("stage whatsapp message: %w", err)
	}

	if matched != nil {
		// Best-effort: failing to tombstone a duplicate must not withhold an
		// ack for a message that is already stored.
		if rows, err := i.commsRepo.SoftDeleteUnmatchedTwin(ctx, syncSource, msg.MessageID); err != nil {
			logger.Warn().Err(err).Str("message_id", msg.MessageID).
				Msg("whatsapp: failed to tombstone the unmatched twin")
		} else if rows > 0 {
			logger.Debug().Int64("rows", rows).Str("message_id", msg.MessageID).
				Msg("whatsapp: tombstoned the unmatched twin of a matched message")
		}
		return nil
	}

	if msg.PeerJID != nil && i.matcher != nil {
		if err := i.matcher.UpdateDiscoveryCandidates(ctx, msg.PeerJID); err != nil {
			logger.Warn().Err(err).Msg("whatsapp: failed to update discovery candidates")
		}
	}
	return nil
}

// resolveContact runs the peer match, tolerating both a nil peer (an outbound
// group message, permanently unmatched by design) and a matcher failure.
func (i *Ingestor) resolveContact(ctx context.Context, msg IngestedMessage) *uuid.UUID {
	if msg.PeerJID == nil || i.matcher == nil {
		return nil
	}
	matched, err := i.matcher.MatchPeer(ctx, *msg.PeerJID, msg.PeerPhoneE164, msg.PushName)
	if err != nil {
		logger.Warn().Err(err).Msg("whatsapp: peer matching failed")
		return nil
	}
	return matched
}

// buildSourceMetadata assembles the JSONB the aggregation adapter reads.
// reply_target_external_id is the key it already looks for.
func buildSourceMetadata(msg IngestedMessage) ([]byte, error) {
	meta := map[string]any{
		"message_type": msg.MessageType,
		"chat_type":    msg.ChatType,
	}
	if msg.PushName != nil {
		meta["push_name"] = *msg.PushName
	}
	if msg.ReplyTargetID != nil {
		meta["reply_target_external_id"] = *msg.ReplyTargetID
	}
	return json.Marshal(meta)
}
