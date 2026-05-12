package aggregation

import (
	"context"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// Message is the source-neutral row the aggregator consumes. The
// per-source adapter (via MessageStore) projects its staging-table row
// into this shape.
//
// InteractionID is the foreign key set once aggregation has processed
// this row — mirrors every per-source staging table's interaction_id
// column. Read by tryExplicitReplyBridge to resolve "the interaction
// the referenced outgoing message produced." It is required at the
// shared layer; without it cross-batch explicit reply bridging silently
// fails.
type Message struct {
	ID            uuid.UUID
	ChatID        string     // source-neutral chat scope key (numeric stringified for sources with numeric chat IDs, opaque guid for sources like Apple Messages)
	IsOutgoing    bool       // direction at the row level
	SentAt        time.Time  // wall clock of the message
	ReplyTargetID *string    // opaque source-defined "external message id" of the referenced message; nil if not a reply
	ExternalID    string     // opaque per-source "first message id" used in source_ref; comparison-by-equality only
	InteractionID *uuid.UUID // FK to interaction once processed; nil for unprocessed rows
}

// SourceAdapter binds a source-name to the aggregator. Concrete adapters
// live in their respective per-source packages; the shared engine
// depends only on this interface.
type SourceAdapter interface {
	// SourceName: the interaction.source constant ("telegram",
	// "messages", "whatsapp"). Used for event envelope Source AND for
	// the interaction.source filter in InteractionFinder.
	SourceName() string

	// SourceRef formats the deterministic burst sourceRef.
	// Telegram returns "tg:<chatID>:<firstExternalID>"; future sources
	// return their own namespace prefix. Aggregator passes (chatID,
	// firstExternalID) lifted from the session's first Message; the
	// adapter formats the string.
	SourceRef(chatID, firstExternalID string) string

	// SourceRefPrefix returns the LIKE-pattern used by InteractionFinder
	// to scope "recent interaction in this chat" queries.
	//
	// CONTRACT: the returned string is consumed as a PostgreSQL `LIKE`
	// pattern. The `%` and `_` characters in chatID are treated as
	// wildcards. Adapters whose chat IDs may legitimately contain `_`
	// or `%` MUST escape the pattern's special characters before
	// returning — recommended escape:
	//   strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(chatID)
	// and switch the underlying SQL to `source_ref LIKE prefix ESCAPE '\'`.
	// Numeric-only chat IDs (e.g. Telegram) do not require escaping.
	SourceRefPrefix(chatID string) string

	// PeerRef formats the event-payload PeerRef field.
	// Telegram returns "tg:<chatID>"; future sources mirror their
	// own namespace.
	PeerRef(chatID string) string

	// Description formats the human-readable interaction description.
	// Telegram returns "Telegram <label> (<n> messages)" where label is
	// "outreach"/"response"/"exchange". Per-source so labels stay
	// accurate ("iMessage", "WhatsApp", etc.) and the existing Telegram
	// phrasing is preserved verbatim.
	Description(direction string, msgCount int) string
}

// MessageStore is the source-neutral staging-IO surface. Concrete
// repositories satisfy this via per-source adapter wrappers (not
// directly) because the shared interface uses Message rows and string
// chat IDs while the underlying per-source repositories use their own
// row types and typed chat IDs. The wrapper does the row-by-row
// conversion and any ID type translation.
//
// ORDERING CONTRACT: ListUnprocessedByContact and
// ListUnprocessedByContactAndChat SHOULD return rows ordered by
// SentAt ascending. Burst/session derivation depends on chronological
// adjacency; the engine sorts defensively to absorb minor adapter
// drift, but adapters that emit grossly out-of-order rows will pay an
// O(N log N) sort tax on every aggregation. Telegram's existing sqlc
// queries already ORDER BY sent_at — preserve that idiom in future
// adapters.
type MessageStore interface {
	ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error)
	ListUnprocessedByContact(ctx context.Context, contactID uuid.UUID) ([]Message, error)
	ListUnprocessedByContactAndChat(ctx context.Context, contactID uuid.UUID, chatID string) ([]Message, error)

	// GetMessageByReplyTarget resolves the message referenced by a
	// reply (Message.ReplyTargetID). The returned Message includes its
	// InteractionID and IsOutgoing so tryExplicitReplyBridge can verify
	// the bridged interaction exists and the message direction is
	// correct.
	//
	// Returns (msg, true, nil) on hit; (Message{}, false, nil) if not
	// found OR if the source-specific ID cannot be parsed; non-nil
	// error only on infrastructure failure.
	GetMessageByReplyTarget(ctx context.Context, chatID, replyTargetID string) (Message, bool, error)

	// MarkProcessed sets processed_at and interaction_id on the rows.
	// The adapter wraps the per-source equivalent query. Non-tx by
	// design; the consumer's tx-bound MarkProcessed dispatch is not
	// part of this interface (it remains source-specific).
	MarkProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error
}

// InteractionFinder locates a "recent matching interaction" for the
// extend/bridge paths. Source-neutral signature; the engine passes
// adapter.SourceName() as source.
type InteractionFinder interface {
	// FindRecentBySourceAndDirection looks up the most recent
	// interaction for (contactID, source, direction) whose source_ref
	// matches the LIKE pattern and whose occurred_at falls in the
	// window. Used by same-direction coalescing.
	FindRecentBySourceAndDirection(
		ctx context.Context,
		contactID uuid.UUID,
		source, direction, sourceRefPrefix string,
		windowStart, windowEnd time.Time,
	) (*repository.Interaction, error)

	// FindRecentOutboundBySource looks up the most recent outbound
	// interaction for (contactID, source) within window. Used by
	// time-based reply bridging on inbound sessions.
	FindRecentOutboundBySource(
		ctx context.Context,
		contactID uuid.UUID,
		source, sourceRefPrefix string,
		windowStart, windowEnd time.Time,
	) (*repository.Interaction, error)

	// GetInteraction passthrough (used by explicit reply bridge to
	// verify direction/source of the bridged interaction).
	GetInteraction(ctx context.Context, id uuid.UUID) (*repository.Interaction, error)
}

// InteractionPromoter promotes an outbound interaction to mutual.
// Satisfied by *service.ContactService via Go's structural typing.
type InteractionPromoter interface {
	PromoteInteractionToMutual(ctx context.Context, interactionID, contactID uuid.UUID, replyAt time.Time) error
}

// InteractionExtender extends an existing interaction's occurred_at and
// description. Satisfied by *service.ContactService via Go's structural
// typing.
type InteractionExtender interface {
	ExtendInteraction(ctx context.Context, interactionID, contactID uuid.UUID, direction string, occurredAt time.Time, description *string) error
}

// EventPublisher emits domain events.
//
// NOTE on nil: callers MUST pass nil literally (not a typed-nil pointer
// like `(*events.Bus)(nil)`) when the publisher is unavailable. The
// engine's "publisher==nil" guard checks the interface value, which a
// typed-nil concrete pointer would satisfy as non-nil. Constructors
// that accept a concrete pointer must convert as
//
//	var pub aggregation.EventPublisher
//	if bus != nil { pub = bus }
//
// so untyped nil propagates. The Engine asserts publisher == nil only
// inside the session create path; modes that intentionally omit the
// bus are refused-with-error there (matching the pre-refactor
// telegram contract).
type EventPublisher interface {
	Publish(ctx context.Context, env *events.Envelope) error
}
