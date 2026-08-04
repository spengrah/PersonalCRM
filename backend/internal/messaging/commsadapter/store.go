package commsadapter

import (
	"context"
	"encoding/json"
	"errors"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReplyTargetMetadataKey is the source_metadata key under which an explicit
// reply's target external message id is stored. The WhatsApp ingest path writes
// it, and the WhatsApp aggregation engine reads it through this projection, so
// the key is live rather than reserved. A source that carries no explicit-reply
// signal simply omits it and the projection's ReplyTargetID stays nil.
const ReplyTargetMetadataKey = "reply_target_external_id"

// StoreAdapter projects repository.CommsMessage rows into aggregation.Message,
// pinning a source. Lives here (not in repository) so the repository package
// stays free of aggregation-package imports. The 7 MessageStore methods
// delegate to the source-parameterized repo methods, passing a.source; the
// ForSource suffix on the repo methods is erased here.
type StoreAdapter struct {
	repo   *repository.CommsMessageRepository
	source string
}

// NewStore returns an aggregation.MessageStore over comms_message scoped to
// source.
func NewStore(repo *repository.CommsMessageRepository, source string) aggregation.MessageStore {
	return &StoreAdapter{repo: repo, source: source}
}

func (a *StoreAdapter) ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error) {
	return a.repo.ListUnprocessedContactIDsForSource(ctx, a.source)
}

func (a *StoreAdapter) ListUnprocessedByContact(ctx context.Context, contactID uuid.UUID) ([]aggregation.Message, error) {
	rows, err := a.repo.ListUnprocessedByContactForSource(ctx, a.source, contactID)
	if err != nil {
		return nil, err
	}
	out := make([]aggregation.Message, len(rows))
	for i := range rows {
		out[i] = MapMessage(rows[i])
	}
	return out, nil
}

func (a *StoreAdapter) ListUnprocessedByContactAndChat(ctx context.Context, contactID uuid.UUID, chatID string) ([]aggregation.Message, error) {
	rows, err := a.repo.ListUnprocessedByContactAndChatForSource(ctx, a.source, contactID, chatID)
	if err != nil {
		return nil, err
	}
	out := make([]aggregation.Message, len(rows))
	for i := range rows {
		out[i] = MapMessage(rows[i])
	}
	return out, nil
}

// GetMessageByReplyTarget resolves the message referenced by a reply. The reply
// target is itself a stored comms_message row, looked up by its own external_id
// within the same (source, contact, chat) scope. comms_message can fan a shared
// address out to one row per matched contact, so the lookup MUST be scoped to
// contactID — otherwise it could return another contact's row whose interaction
// would then be wrongly bridged.
func (a *StoreAdapter) GetMessageByReplyTarget(ctx context.Context, contactID uuid.UUID, chatID, replyTargetID string) (aggregation.Message, bool, error) {
	row, err := a.repo.GetMessageByReplyTargetForSource(ctx, a.source, contactID, chatID, replyTargetID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return aggregation.Message{}, false, nil
		}
		return aggregation.Message{}, false, err
	}
	return MapMessage(*row), true, nil
}

func (a *StoreAdapter) MarkProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	return a.repo.MarkMessagesProcessed(ctx, messageIDs, interactionID)
}

func (a *StoreAdapter) ClaimRowsTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	return a.repo.ClaimMessagesTx(ctx, tx, messageIDs, sessionRef)
}

func (a *StoreAdapter) ClearStaleClaimTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	return a.repo.ClearStaleClaimTx(ctx, tx, messageIDs, expectedSessionRef)
}

// MapMessage projects a repository.CommsMessage into the source-neutral
// aggregation.Message. ChatID comes from thread_id (the chat scope key); a nil
// thread_id defensively yields ChatID == "" (chat sources always write it
// non-null). Preserving InteractionID / ClaimedAt / ClaimedSessionRef is
// required for cross-batch explicit reply bridging and stale-claim /
// boundary-shift recovery. ReplyTargetID is parsed from
// source_metadata.reply_target_external_id when present as a non-empty string.
//
// Exported so per-source tests can assert the projection against their own
// rows.
func MapMessage(m repository.CommsMessage) aggregation.Message {
	out := aggregation.Message{
		ID:                m.ID,
		IsOutgoing:        m.Direction == repository.InteractionDirectionOutbound,
		SentAt:            m.SentAt,
		ExternalID:        m.ExternalID,
		InteractionID:     m.InteractionID,
		ClaimedAt:         m.ClaimedAt,
		ClaimedSessionRef: m.ClaimedSessionRef,
	}
	if m.ThreadID != nil {
		out.ChatID = *m.ThreadID
	}
	if rt := parseReplyTargetID(m.SourceMetadata); rt != "" {
		out.ReplyTargetID = &rt
	}
	return out
}

// parseReplyTargetID extracts source_metadata.reply_target_external_id as a
// non-empty string, or "" when the key is absent, empty, or the wrong type.
func parseReplyTargetID(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	v, ok := meta[ReplyTargetMetadataKey]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}
