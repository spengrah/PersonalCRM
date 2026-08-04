package repository

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// VenueContainer is the per-source container identity a recorder resolves an
// interaction's venue from. Kind + ContainerID map to the venue
// (source, kind, source_container_id) unique; Title is the human label.
type VenueContainer struct {
	Kind        string
	ContainerID string
	Title       string
}

// VenueContainerReader resolves a message.* staging row's container key from a
// staging row id, inside the caller's tx. One implementation per chat source
// (telegram, messages, gchat). The recorder has the staging row ids in hand
// (the same MessageIDs it marks processed), so the container is one read away.
type VenueContainerReader interface {
	ContainerForMessageTx(ctx context.Context, tx pgx.Tx, messageID uuid.UUID) (VenueContainer, error)
}

// gcalEventTupleReader reads the (gcal_event_id, gcal_calendar_id,
// google_account_id) 3-tuple + title off a calendar_event by its internal id,
// inside the caller's tx. Satisfied by *CalendarEventRepository.GetByIDTx.
type gcalEventTupleReader interface {
	GetByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*CalendarEvent, error)
}

// VenueResolverRegistry routes a recorder's container lookup to the right
// per-source resolution path, then resolves the venue node id (creating it on
// first sight). For message.* it dispatches by source to a per-source reader;
// for gcal it reads the calendar_event 3-tuple. Mirrors
// StagingProcessorRegistry's one-entry-per-source shape.
//
// Resolution is best-effort wrt the interaction insert: an unknown source, an
// empty id list, or a missing/empty container yields a nil id + nil error so
// the recorder records the interaction with a NULL venue_id rather than
// failing. The interaction row is the durable contract; the venue link is an
// enrichment.
type VenueResolverRegistry struct {
	venues   *VenueRepository
	readers  map[string]VenueContainerReader
	calendar gcalEventTupleReader
}

// NewVenueResolverRegistry builds the registry from a source → reader map, the
// venue repository used to create/find the venue node, and the calendar-event
// reader used for the gcal 3-tuple. calendar may be nil in tests that don't
// exercise gcal.
func NewVenueResolverRegistry(venues *VenueRepository, readers map[string]VenueContainerReader, calendar gcalEventTupleReader) *VenueResolverRegistry {
	return &VenueResolverRegistry{venues: venues, readers: readers, calendar: calendar}
}

// ResolveMessageVenueTx resolves the venue node id for a message.* interaction
// from its staging row ids. Returns (nil, nil) — record with NULL venue_id —
// for an unknown source, an empty id list, or an unresolvable container; only a
// real DB error propagates.
func (r *VenueResolverRegistry) ResolveMessageVenueTx(
	ctx context.Context, tx pgx.Tx, source string, messageIDs []uuid.UUID,
) (*uuid.UUID, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	reader, ok := r.readers[source]
	if !ok {
		return nil, nil
	}
	container, err := reader.ContainerForMessageTx(ctx, tx, messageIDs[0])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Staging row gone (hard-deleted in a test, or a race) — record
			// without a venue rather than fail the interaction.
			return nil, nil
		}
		return nil, err
	}
	if container.ContainerID == "" {
		return nil, nil
	}
	nodeID, err := r.venues.ResolveVenueForInteraction(ctx, tx, source, container.Kind, container.ContainerID, container.Title)
	if err != nil {
		return nil, err
	}
	return &nodeID, nil
}

// ResolveVenueForInteractionTx resolves (creating on first sight) the venue node
// id for a container the caller already has in hand — phone calls and anarlog
// sessions, which carry their container key directly. Passthrough to the venue
// repository so service-layer recorders depend on this one registry rather than
// the repo + the registry separately.
func (r *VenueResolverRegistry) ResolveVenueForInteractionTx(
	ctx context.Context, tx pgx.Tx, source, kind, containerKey, title string,
) (uuid.UUID, error) {
	return r.venues.ResolveVenueForInteraction(ctx, tx, source, kind, containerKey, title)
}

// ResolveGCalVenueTx resolves the meeting venue for a gcal interaction from the
// internal calendar_event id (the interaction's source_ref). Returns (nil, nil)
// — record with NULL venue_id — when the calendar row is gone or the reader is
// unwired; only a real DB error propagates.
func (r *VenueResolverRegistry) ResolveGCalVenueTx(ctx context.Context, tx pgx.Tx, calendarEventID uuid.UUID) (*uuid.UUID, error) {
	if r.calendar == nil {
		return nil, nil
	}
	event, err := r.calendar.GetByIDTx(ctx, tx, calendarEventID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	container := GCalVenueContainerID(event.GcalEventID, event.GcalCalendarID, event.GoogleAccountID)
	var title string
	if event.Title != nil {
		title = *event.Title
	}
	nodeID, err := r.venues.ResolveVenueForInteraction(ctx, tx, InteractionSourceGCal, VenueKindMeeting, container, title)
	if err != nil {
		return nil, err
	}
	return &nodeID, nil
}

// GCalVenueContainerID builds the gcal venue container key — the length-prefixed
// (gcal_event_id, gcal_calendar_id, google_account_id) 3-tuple. The 3-tuple is
// calendar_event's real uniqueness; length-prefixing prevents a delimiter in any
// component from aliasing a different tuple. MUST stay byte-identical to the 069
// migration's gcal container expression.
func GCalVenueContainerID(gcalEventID, gcalCalendarID, googleAccountID string) string {
	return lengthPrefix(gcalEventID) + "|" + lengthPrefix(gcalCalendarID) + "|" + lengthPrefix(googleAccountID)
}

// --- per-source readers ---

// TelegramVenueContainerReader reads the telegram chat container off a staging
// row. chat_type 'private' is a DM; group/supergroup are group chats.
type TelegramVenueContainerReader struct{}

// NewTelegramVenueContainerReader builds the reader.
func NewTelegramVenueContainerReader() *TelegramVenueContainerReader {
	return &TelegramVenueContainerReader{}
}

// ContainerForMessageTx implements VenueContainerReader for telegram.
func (r *TelegramVenueContainerReader) ContainerForMessageTx(ctx context.Context, tx pgx.Tx, messageID uuid.UUID) (VenueContainer, error) {
	row, err := db.New(tx).GetTelegramMessageContainer(ctx, uuidToPgUUID(messageID))
	if err != nil {
		return VenueContainer{}, err
	}
	kind := VenueKindGroupChat
	if row.ChatType == "private" {
		kind = VenueKindDM
	}
	var title string
	if row.ChatTitle.Valid {
		title = row.ChatTitle.String
	}
	return VenueContainer{
		Kind:        kind,
		ContainerID: strconv.FormatInt(row.TelegramChatID, 10),
		Title:       title,
	}, nil
}

// MessagesVenueContainerReader reads the iMessage chat container off a staging
// row. is_group_chat decides group_chat vs dm.
type MessagesVenueContainerReader struct{}

// NewMessagesVenueContainerReader builds the reader.
func NewMessagesVenueContainerReader() *MessagesVenueContainerReader {
	return &MessagesVenueContainerReader{}
}

// ContainerForMessageTx implements VenueContainerReader for messages.
func (r *MessagesVenueContainerReader) ContainerForMessageTx(ctx context.Context, tx pgx.Tx, messageID uuid.UUID) (VenueContainer, error) {
	row, err := db.New(tx).GetMessagesMessageContainer(ctx, uuidToPgUUID(messageID))
	if err != nil {
		return VenueContainer{}, err
	}
	kind := VenueKindDM
	if row.IsGroupChat {
		kind = VenueKindGroupChat
	}
	return VenueContainer{
		Kind:        kind,
		ContainerID: row.ChatGuid,
	}, nil
}

// GChatVenueContainerReader reads the Google Chat space/thread container off a
// comms_message staging row.
type GChatVenueContainerReader struct{}

// NewGChatVenueContainerReader builds the reader.
func NewGChatVenueContainerReader() *GChatVenueContainerReader {
	return &GChatVenueContainerReader{}
}

// ContainerForMessageTx implements VenueContainerReader for gchat.
func (r *GChatVenueContainerReader) ContainerForMessageTx(ctx context.Context, tx pgx.Tx, messageID uuid.UUID) (VenueContainer, error) {
	row, err := db.New(tx).GetCommsMessageContainer(ctx, uuidToPgUUID(messageID))
	if err != nil {
		return VenueContainer{}, err
	}
	var containerID string
	if row.ThreadID.Valid {
		containerID = row.ThreadID.String
	}
	return VenueContainer{
		Kind:        VenueKindGroupChat,
		ContainerID: containerID,
	}, nil
}

// whatsappGroupJIDSuffix is the server part of a WhatsApp group JID
// (`<digits>-<digits>@g.us`). It is the ONLY discriminator the reader needs:
// the ingest path's person-to-person allowlist refuses every chat server except
// the group server and the two user servers (`s.whatsapp.net`, `lid`), so a
// thread id that is not a group JID is necessarily a direct chat.
const whatsappGroupJIDSuffix = "@g.us"

// WhatsAppVenueContainerReader reads the WhatsApp chat container off a
// comms_message staging row. Unlike GChat (whose spaces are always group
// chats), WhatsApp carries both kinds, so the kind is derived from the chat
// JID's server suffix.
type WhatsAppVenueContainerReader struct{}

// NewWhatsAppVenueContainerReader builds the reader.
func NewWhatsAppVenueContainerReader() *WhatsAppVenueContainerReader {
	return &WhatsAppVenueContainerReader{}
}

// ContainerForMessageTx implements VenueContainerReader for whatsapp.
func (r *WhatsAppVenueContainerReader) ContainerForMessageTx(ctx context.Context, tx pgx.Tx, messageID uuid.UUID) (VenueContainer, error) {
	row, err := db.New(tx).GetCommsMessageContainer(ctx, uuidToPgUUID(messageID))
	if err != nil {
		return VenueContainer{}, err
	}
	var containerID string
	if row.ThreadID.Valid {
		containerID = row.ThreadID.String
	}
	kind := VenueKindDM
	if strings.HasSuffix(containerID, whatsappGroupJIDSuffix) {
		kind = VenueKindGroupChat
	}
	return VenueContainer{
		Kind:        kind,
		ContainerID: containerID,
	}, nil
}
