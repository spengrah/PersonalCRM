// Package events defines the typed event kinds and Envelope passed through
// the event bus. See .ai/spec/event-bus-foundation.md §3.2.
package events

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
)

// Kind is a dot-namespaced event kind (spec §3.1 notes on kind namespacing).
type Kind string

const (
	// Raw-signal events — published by provider/publisher code.
	KindMessageReceived      Kind = "message.received"
	KindMessageSent          Kind = "message.sent"
	KindCalendarAttended     Kind = "calendar.attended"
	KindCalendarDeclined     Kind = "calendar.declined"
	KindTaskCompleted        Kind = "task.completed"
	KindTaskSkipped          Kind = "task.skipped"
	KindTaskOutreachDetected Kind = "task.outreach_detected"
	KindInteractionManual    Kind = "interaction.manual"
	KindContactMethodsAdded  Kind = "contact_methods.added"

	// Derived event — emitted by the InteractionRecorder consumer (spec
	// §3.4.1) atomically with an interaction row insert. PR 2 declares
	// the constant; PR 5 introduces the consumer that emits it.
	KindInteractionRecorded Kind = "interaction.recorded"

	// Daemon-emitted raw-message events. Published by the Mac daemon
	// against /api/v1/ingest/events with host-auth. NOT consumed by any
	// bus consumer — IngestService processes them inline within the
	// per-event savepoint (identity match → staging upsert → aggregator
	// enqueue). The event-log row exists for audit + (source, source_id)
	// dedup on retries.
	KindRawMessageReceived Kind = "raw_message.received"
	KindRawMessageSent     Kind = "raw_message.sent"

	// Daemon-emitted external-contact events. Published by the Mac
	// daemon's iCloud Contacts watcher against /api/v1/ingest/events
	// with host-auth. NOT consumed by any bus consumer — IngestService
	// processes them inline within the per-event savepoint (identity
	// match per email/phone → external_contact upsert/tombstone with
	// optional revive). The event-log row exists for audit +
	// (source, source_id) dedup on retries.
	KindExternalContactUpserted Kind = "external_contact.upserted"
	KindExternalContactDeleted  Kind = "external_contact.deleted"

	// Daemon-emitted meeting-note (anarlog session) events. Published
	// by the Mac daemon's anarlog_sessions plugin. NOT consumed by any
	// bus consumer — IngestService processes them inline within the
	// per-event savepoint (linkage-detection algorithm: calendar window
	// match, tagged-participant resolve, walk-in supplemental). See
	// .ai/spec/mac-daemon-phase-2-anarlog-matching.md for the full
	// linkage state machine.
	KindMeetingNoteRecorded Kind = "meeting_note.recorded"
	KindMeetingNoteDeleted  Kind = "meeting_note.deleted"

	// Daemon-emitted phone-call events. Published by the Mac daemon's
	// CallHistoryDB watcher against /api/v1/ingest/events with
	// host-auth. NOT consumed by any bus consumer — IngestService
	// processes them inline within the per-event savepoint
	// (identity match → phone_call staging upsert → optional
	// interaction insert + cadence apply, per the content-delivered
	// decision table; spec §`phone_calls` source). The event-log row
	// exists for audit + (source, source_id) dedup on retries.
	KindCallReceived Kind = "call.received"
	KindCallSent     Kind = "call.sent"
)

// AllKinds enumerates every defined Kind. Used by tests to guard that
// consumerJobsForKind and kindPayloadTypes cover every kind. Extend when
// adding a new Kind.
var AllKinds = []Kind{
	KindMessageReceived,
	KindMessageSent,
	KindCalendarAttended,
	KindCalendarDeclined,
	KindTaskCompleted,
	KindTaskSkipped,
	KindTaskOutreachDetected,
	KindInteractionManual,
	KindContactMethodsAdded,
	KindInteractionRecorded,
	KindRawMessageReceived,
	KindRawMessageSent,
	KindExternalContactUpserted,
	KindExternalContactDeleted,
	KindMeetingNoteRecorded,
	KindMeetingNoteDeleted,
	KindCallReceived,
	KindCallSent,
}

// Envelope is the wire/DB shape passed through Bus.Publish. Payload is
// kind-specific; use Marshal / Unmarshal to round-trip through typed
// payload structs.
type Envelope struct {
	ID         uuid.UUID       `json:"id"`
	Source     string          `json:"source"`
	SourceID   string          `json:"source_id,omitempty"`
	Kind       Kind            `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	ObservedAt time.Time       `json:"observed_at"`
}

// Per-kind typed payloads. Each carries Version int to enable forward
// migrations. Adding fields to an existing struct is non-breaking; a
// breaking change introduces a V2 struct and version-dispatches in the
// consumer.

// MessageReceivedPayload is the payload for KindMessageReceived.
//
// Direction is an optional publisher-set override. When non-empty the
// consumer uses it verbatim (e.g., telegram "fresh-mutual" sessions
// classified at publish time emit a KindMessageReceived with
// Direction="mutual"). When empty the consumer applies the kind default
// ("inbound" for received). See plan Decision 6.
type MessageReceivedPayload struct {
	Version           int        `json:"version"`              // start at 1
	ContactID         *uuid.UUID `json:"contact_id,omitempty"` // nil if unmatched peer
	PeerRef           string     `json:"peer_ref"`             // e.g. "tg:12345:67890"
	MessageAt         time.Time  `json:"message_at"`
	Description       *string    `json:"description,omitempty"`
	ExternalMessageID string     `json:"external_message_id,omitempty"`
	Direction         string     `json:"direction,omitempty"` // optional override; empty = kind default "inbound"
	// MessageIDs carries the underlying telegram_message UUIDs that make up
	// this aggregated session. Populated by the Telegram aggregation
	// publisher in cutover mode so the consumer can mark the rows
	// processed (telegram_message.interaction_id FK) inside the same tx
	// that inserts the interaction row (spec §3.4.1; plan Decision 10).
	// Non-telegram publishers leave this empty.
	MessageIDs []uuid.UUID `json:"message_ids,omitempty"`
}

// MessageSentPayload is the payload for KindMessageSent.
//
// Direction is an optional publisher-set override. Empty = kind default
// "outbound". Non-empty value wins (e.g., fresh-mutual → "mutual").
// See plan Decision 6.
type MessageSentPayload struct {
	Version           int        `json:"version"`
	ContactID         *uuid.UUID `json:"contact_id,omitempty"`
	PeerRef           string     `json:"peer_ref"`
	MessageAt         time.Time  `json:"message_at"`
	Description       *string    `json:"description,omitempty"`
	ExternalMessageID string     `json:"external_message_id,omitempty"`
	Direction         string     `json:"direction,omitempty"` // optional override; empty = kind default "outbound"
	// MessageIDs carries the underlying telegram_message UUIDs — see
	// MessageReceivedPayload.MessageIDs for full docs.
	MessageIDs []uuid.UUID `json:"message_ids,omitempty"`
}

// CalendarAttendedPayload is the payload for KindCalendarAttended.
//
// Title carries the calendar event's summary/title so the consumer can
// populate interaction.description. Pre-cutover (PR 5) the direct-path
// RecordInteraction call passed the title inline; post-cutover the
// consumer is the sole writer and needs the title via the payload.
// Optional (nil when the calendar event has no title).
type CalendarAttendedPayload struct {
	Version    int       `json:"version"`
	ContactID  uuid.UUID `json:"contact_id"`
	EventID    string    `json:"event_id"` // GCal event id
	OccurredAt time.Time `json:"occurred_at"`
	Title      *string   `json:"title,omitempty"`
}

// CalendarDeclinedPayload is the payload for KindCalendarDeclined.
type CalendarDeclinedPayload struct {
	Version    int       `json:"version"`
	ContactID  uuid.UUID `json:"contact_id"`
	EventID    string    `json:"event_id"`
	OccurredAt time.Time `json:"occurred_at"`
}

// TaskCompletedPayload is the payload for KindTaskCompleted. Direction
// distinguishes outbound vs mutual completion semantics: reach_out and
// send completions emit "outbound"; legacy action / meet tasks emit
// "mutual" to preserve historical interaction semantics.
//
// V2 adds SuppressFollowUp: when true the InteractionRecorder propagates
// the flag to InteractionRecordedPayload V3 so FollowUpManager
// short-circuits without spawning a successor follow-up. Polarity is
// "zero=safe": the zero value (false) preserves the legacy spawn-a-
// follow-up default; only kind=send completions emit true.
type TaskCompletedPayload struct {
	Version          int       `json:"version"`
	ContactID        uuid.UUID `json:"contact_id"`
	TaskID           string    `json:"task_id"`   // external provider id (e.g., todoist)
	TaskKind         string    `json:"task_kind"` // post-046: reach_out | send | reminder | meet | action
	CompletedAt      time.Time `json:"completed_at"`
	Direction        string    `json:"direction"`                    // "outbound" | "mutual"
	SuppressFollowUp bool      `json:"suppress_follow_up,omitempty"` // V2+; zero=do-not-suppress
}

// TaskSkippedPayload is the payload for KindTaskSkipped.
type TaskSkippedPayload struct {
	Version   int       `json:"version"`
	ContactID uuid.UUID `json:"contact_id"`
	TaskID    string    `json:"task_id"`
	SkippedAt time.Time `json:"skipped_at"`
}

// TaskOutreachDetectedPayload is the payload for KindTaskOutreachDetected.
type TaskOutreachDetectedPayload struct {
	Version    int       `json:"version"`
	ContactID  uuid.UUID `json:"contact_id"`
	TaskID     string    `json:"task_id"`
	DetectedAt time.Time `json:"detected_at"`
}

// InteractionManualPayload is the payload for KindInteractionManual.
type InteractionManualPayload struct {
	Version     int       `json:"version"`
	ContactID   uuid.UUID `json:"contact_id"`
	Direction   string    `json:"direction"` // "outbound" | "inbound" | "mutual"
	OccurredAt  time.Time `json:"occurred_at"`
	Description string    `json:"description,omitempty"`
}

// ContactMethodRef is a normalized reference to a contact method used in
// ContactMethodsAddedPayload (spec §3.4.4).
type ContactMethodRef struct {
	Type  string `json:"type"`  // email | phone | telegram | ...
	Value string `json:"value"` // normalized
}

// ContactMethodsAddedPayload is the payload for KindContactMethodsAdded.
// ONE event per mutation, carrying ALL newly-added methods — see spec §3.4.4
// for rationale (avoids N-job cross-mutation collapse).
type ContactMethodsAddedPayload struct {
	Version      int                `json:"version"`
	ContactID    uuid.UUID          `json:"contact_id"`
	Methods      []ContactMethodRef `json:"methods"`
	RematchJobID uuid.UUID          `json:"rematch_job_id"`
}

// InteractionRecordedPayload is the payload for KindInteractionRecorded,
// emitted by the InteractionRecorder consumer in PR 5+. PR 7 bumps
// Version to 2 and adds the PrevCadenceSnapshot + PrevCadenceValue
// fields so CadenceUpdater can replay the direct-path's pre-cadence
// state deterministically (plan Decision 2a). V1 payloads continue to
// unmarshal — the new fields default to nil and InteractionRecorder
// (the only PR 6 consumer of this event) ignores them. PR 7's
// CadenceUpdater rejects V1 payloads at ERROR level.
type InteractionRecordedPayload struct {
	Version       int       `json:"version"`
	ContactID     uuid.UUID `json:"contact_id"`
	InteractionID uuid.UUID `json:"interaction_id"`
	Direction     string    `json:"direction"`
	OccurredAt    time.Time `json:"occurred_at"`
	Source        string    `json:"source"`
	SourceRef     *string   `json:"source_ref,omitempty"`

	// PrevCadenceSnapshot carries the four cadence-column values at the
	// moment the direct-path UPDATE ran. CadenceUpdater uses this as
	// prev_consumer so its math is deterministic vs. the direct path
	// even if contact state mutates between emit and async consume
	// (plan Decision 2a). Nil on V1 payloads; required on V2.
	PrevCadenceSnapshot *CadenceFieldsSnapshot `json:"prev_cadence_snapshot,omitempty"`

	// PrevCadenceValue is contact.cadence at emit time — the cadence
	// string (e.g., "weekly") that the direct path used to compute
	// contact_by. Consumer prefers this over a live re-read so a
	// cadence edit between emit and consume doesn't cause divergence.
	// Nil on V1 payloads and when the contact has no cadence set.
	PrevCadenceValue *string `json:"prev_cadence_value,omitempty"`

	// SuppressFollowUp is propagated from TaskCompletedPayload V2 by
	// InteractionRecorder. When true, FollowUpManager short-circuits
	// without spawning a successor follow-up. Polarity matches the
	// upstream payload: zero (false) = "do not suppress" = legacy
	// spawn-a-follow-up. Only kind=send completions set this to true.
	// V3+; on V1/V2 payloads it decodes to false.
	SuppressFollowUp bool `json:"suppress_follow_up,omitempty"`
}

// CadenceFieldsSnapshot is the four-cadence-column snapshot embedded in
// InteractionRecordedPayload (V2+). See plan Decision 2a.
type CadenceFieldsSnapshot struct {
	LastContacted  *time.Time `json:"last_contacted,omitempty"`
	LastOutreachAt *time.Time `json:"last_outreach_at,omitempty"`
	LastResponseAt *time.Time `json:"last_response_at,omitempty"`
	ContactBy      *time.Time `json:"contact_by,omitempty"` // date-precision
}

// AttachmentMeta carries metadata-only descriptors for a message
// attachment. No content; the daemon never ships attachment bytes
// across the wire. Defined alongside RawMessage*Payload so the payload
// struct compiles today; the field is `omitempty` so daemon clients
// that don't yet ship attachments produce wire-compatible payloads.
type AttachmentMeta struct {
	Type     string  `json:"type"` // photo|audio|video|document|other
	Filename *string `json:"filename,omitempty"`
	MimeType *string `json:"mime_type,omitempty"`
	Size     *int64  `json:"size,omitempty"`
}

// canonicalMessageTypes is the set of values accepted by
// messages_message.message_type CHECK constraint. Matched verbatim
// against the migration so payload validation rejects bad values
// before the CHECK constraint aborts the ingest tx.
var canonicalMessageTypes = map[string]struct{}{
	"text":     {},
	"photo":    {},
	"audio":    {},
	"video":    {},
	"document": {},
	"other":    {},
}

// IsCanonicalMessageType reports whether t is one of the six allowed
// message_type values for messages_message. Used by ValidatePayload
// for raw_message.* kinds and by the handler's pre-service validator.
func IsCanonicalMessageType(t string) bool {
	_, ok := canonicalMessageTypes[t]
	return ok
}

// RawMessageReceivedPayload is the payload for KindRawMessageReceived,
// emitted by the Mac daemon. Carries the full raw data needed by the
// inline ingest handler to identity-match, upsert staging, and enqueue
// the aggregator job. NOT consumed by any bus consumer — processed
// inline within the ingest transaction.
//
// Version evolution: per spec §3.2, additive fields are V1-compatible;
// breaking changes ship a V2 struct.
type RawMessageReceivedPayload struct {
	Version     int              `json:"version"`             // start at 1
	HostID      uuid.UUID        `json:"host_id"`             // authenticated host that observed the row
	Source      string           `json:"source"`              // currently "messages"
	Guid        string           `json:"guid"`                // iMessage guid; primary dedup key
	ChatID      string           `json:"chat_id"`             // chat.db chat.guid
	PeerHandle  string           `json:"peer_handle"`         // raw phone/email as observed in chat.db
	PeerName    *string          `json:"peer_name,omitempty"` // sender's display name when available
	Text        *string          `json:"text,omitempty"`      // full message body
	MessageType string           `json:"message_type"`        // text|photo|audio|video|document|other
	IsGroup     bool             `json:"is_group"`            // group chat vs DM
	SentAt      time.Time        `json:"sent_at"`             // from source, NOT daemon clock
	ReplyToGuid *string          `json:"reply_to_guid,omitempty"`
	Attachments []AttachmentMeta `json:"attachments,omitempty"` // metadata only; daemon populates when chat.db reader lands
}

// RawMessageSentPayload is the payload for KindRawMessageSent. Same
// shape as RawMessageReceivedPayload (the only difference is the kind
// constant — used by the inline handler to set IsOutgoing on staging).
type RawMessageSentPayload RawMessageReceivedPayload

// ExternalContactMethodValue carries a single contact-method value
// (email or phone) plus its display tags. Same shape used for both
// emails[] and phones[] in ExternalContactUpsertedPayload.
type ExternalContactMethodValue struct {
	Value   string `json:"value"`             // raw, daemon-supplied
	Type    string `json:"type,omitempty"`    // e.g. "home", "work"
	Primary bool   `json:"primary,omitempty"` // true on the entity's primary
}

// ExternalContactAddressValue is the address shape in
// ExternalContactUpsertedPayload. Metadata-only (no geocoding).
type ExternalContactAddressValue struct {
	Formatted string `json:"formatted"`
	Type      string `json:"type,omitempty"`
}

// ExternalContactUpsertedPayload is the payload for
// KindExternalContactUpserted, emitted by the Mac daemon on every
// CNChangeHistoryFetchRequest add/update event for a contact in an
// allow-listed container. Inline-processed by IngestService (identity
// match per email/phone → external_contact upsert; revives a
// tombstoned row when present).
//
// Version starts at 1. Additive fields are V1-compatible.
//
// Metadata is source-specific: the Mac daemon populates
// `container_identifier` (which CNContainer this contact came from);
// future sources populate their own keys. Persisted into
// external_contact.metadata JSONB wholesale (NOT merged) — daemon owns
// the contents.
type ExternalContactUpsertedPayload struct {
	Version      int                           `json:"version"`   // start at 1
	HostID       uuid.UUID                     `json:"host_id"`   // authenticated host
	Source       string                        `json:"source"`    // e.g. "icloud_contacts"
	EntityID     string                        `json:"entity_id"` // CNContact identifier; column source_id
	DisplayName  *string                       `json:"display_name,omitempty"`
	FirstName    *string                       `json:"first_name,omitempty"`
	LastName     *string                       `json:"last_name,omitempty"`
	Emails       []ExternalContactMethodValue  `json:"emails,omitempty"`
	Phones       []ExternalContactMethodValue  `json:"phones,omitempty"`
	Addresses    []ExternalContactAddressValue `json:"addresses,omitempty"`
	Organization *string                       `json:"organization,omitempty"`
	JobTitle     *string                       `json:"job_title,omitempty"`
	Birthday     *string                       `json:"birthday,omitempty"` // ISO date YYYY-MM-DD
	PhotoURL     *string                       `json:"photo_url,omitempty"`
	Metadata     map[string]any                `json:"metadata,omitempty"`
}

// ExternalContactDeletedPayload is the payload for
// KindExternalContactDeleted, emitted on a CNChangeHistoryFetchRequest
// delete (or tombstone reconciliation on a full resync). The handler
// tombstones the row by id; an unknown entity is a silent no-op
// (event-log row is durable for audit either way).
type ExternalContactDeletedPayload struct {
	Version  int       `json:"version"`   // start at 1
	HostID   uuid.UUID `json:"host_id"`   // authenticated host
	Source   string    `json:"source"`    // e.g. "icloud_contacts"
	EntityID string    `json:"entity_id"` // CNContact identifier
}

// MeetingNoteRecordedPayload is the payload for KindMeetingNoteRecorded,
// emitted by the Mac daemon's anarlog_sessions plugin on a session
// add/update. Inline-processed by IngestService (linkage detection +
// re-sync diff + needs_attention surfacing). NOT consumed by any bus
// consumer.
//
// SourceID is the anarlog session UUID (the envelope's source_id wraps
// this with the content-hash suffix per spec line 362). ParticipantIDs
// are anarlog_human UUIDs that the handler resolves to CRM contacts via
// the external_identity row planted by the anarlog_humans path.
type MeetingNoteRecordedPayload struct {
	Version        int       `json:"version"`   // start at 1
	HostID         uuid.UUID `json:"host_id"`   // authenticated host
	Source         string    `json:"source"`    // "anarlog_sessions"
	SourceID       string    `json:"source_id"` // anarlog session UUID
	Title          *string   `json:"title,omitempty"`
	MeetingAt      time.Time `json:"meeting_at"`
	Summary        *string   `json:"summary,omitempty"`
	Memo           *string   `json:"memo,omitempty"`
	ParticipantIDs []string  `json:"participant_ids,omitempty"` // anarlog_human UUIDs
	Tags           []string  `json:"tags,omitempty"`
}

// MeetingNoteDeletedPayload is the payload for KindMeetingNoteDeleted,
// emitted on a session delete (or tombstone reconciliation). The
// handler soft-deletes the meeting_note row AND explicitly soft-deletes
// every session-attributed interaction (since UPDATE deleted_at does
// NOT trigger ON DELETE CASCADE). Unknown session UUID → silent no-op.
type MeetingNoteDeletedPayload struct {
	Version  int       `json:"version"`   // start at 1
	HostID   uuid.UUID `json:"host_id"`   // authenticated host
	Source   string    `json:"source"`    // "anarlog_sessions"
	SourceID string    `json:"source_id"` // anarlog session UUID
}

// canonicalCallServices is the set of values accepted by phone_call.service
// CHECK constraint. Frozen per spec §`phone_calls` source (settled decision
// S4). Matched verbatim against the migration so payload validation rejects
// bad values before the CHECK constraint aborts the ingest tx.
var canonicalCallServices = map[string]struct{}{
	"voice":          {},
	"facetime_audio": {},
	"facetime_video": {},
}

// IsCanonicalCallService reports whether s is one of the three allowed
// phone_call.service values. Used by ValidatePayload for call.* kinds.
func IsCanonicalCallService(s string) bool {
	_, ok := canonicalCallServices[s]
	return ok
}

// CallPayload is the payload for KindCallReceived and KindCallSent,
// emitted by the Mac daemon's CallHistoryDB watcher. One struct serves
// both kinds; the discriminator is the kind, and the inline ingest
// handler cross-checks Direction against the kind for defence-in-depth.
//
// peer_normalized is the canonicalized form of peer_handle (E.164 phone or
// lowercased email). The daemon canonicalizes; the Pi re-canonicalizes
// defensively in verifyCallInvariants before trusting the value.
//
// Answered is *bool because the column is three-state: NULL for outbound
// (ZANSWERED is empirically unreliable on outbound rows), true/false for
// inbound. The daemon forces NULL outbound regardless of source data;
// the Pi re-normalizes for defence in depth.
//
// HasVoicemail is forced FALSE for outbound by both the daemon and the
// Pi (the column is semantically inbound-only; iOS reuses it for
// outbound greeting markers which we deliberately don't surface).
type CallPayload struct {
	Version         int       `json:"version"`            // start at 1
	HostID          uuid.UUID `json:"host_id"`            // authenticated host that observed the row
	Source          string    `json:"source"`             // currently "phone_calls"
	CallUniqueID    string    `json:"call_unique_id"`     // ZCALLRECORD.ZUNIQUE_ID; primary dedup key
	PeerHandle      string    `json:"peer_handle"`        // raw ZADDRESS as observed in CallHistoryDB
	PeerNormalized  string    `json:"peer_normalized"`    // canonicalized via HandleNormalization
	Service         string    `json:"service"`            // voice | facetime_audio | facetime_video
	Direction       string    `json:"direction"`          // inbound | outbound (mirrors kind)
	Answered        *bool     `json:"answered,omitempty"` // inbound only; omit/NULL for outbound
	HasVoicemail    bool      `json:"has_voicemail"`      // always false for outbound
	DurationSeconds int32     `json:"duration_seconds"`   // ZDURATION; 0 for missed
	StartedAt       time.Time `json:"started_at"`         // ZDATE converted to UTC
}

// kindPayloadTypes is the canonical Kind → payload-type registry used by
// Marshal and Unmarshal to assert type-vs-kind consistency at runtime. Add
// a row each time a new Kind + payload struct are introduced. Keep in sync
// with AllKinds (the unit test TestKindPayloadTypes_CoversAllKinds enforces
// this invariant).
var kindPayloadTypes = map[Kind]reflect.Type{
	KindMessageReceived:         reflect.TypeOf(MessageReceivedPayload{}),
	KindMessageSent:             reflect.TypeOf(MessageSentPayload{}),
	KindCalendarAttended:        reflect.TypeOf(CalendarAttendedPayload{}),
	KindCalendarDeclined:        reflect.TypeOf(CalendarDeclinedPayload{}),
	KindTaskCompleted:           reflect.TypeOf(TaskCompletedPayload{}),
	KindTaskSkipped:             reflect.TypeOf(TaskSkippedPayload{}),
	KindTaskOutreachDetected:    reflect.TypeOf(TaskOutreachDetectedPayload{}),
	KindInteractionManual:       reflect.TypeOf(InteractionManualPayload{}),
	KindContactMethodsAdded:     reflect.TypeOf(ContactMethodsAddedPayload{}),
	KindInteractionRecorded:     reflect.TypeOf(InteractionRecordedPayload{}),
	KindRawMessageReceived:      reflect.TypeOf(RawMessageReceivedPayload{}),
	KindRawMessageSent:          reflect.TypeOf(RawMessageSentPayload{}),
	KindExternalContactUpserted: reflect.TypeOf(ExternalContactUpsertedPayload{}),
	KindExternalContactDeleted:  reflect.TypeOf(ExternalContactDeletedPayload{}),
	KindMeetingNoteRecorded:     reflect.TypeOf(MeetingNoteRecordedPayload{}),
	KindMeetingNoteDeleted:      reflect.TypeOf(MeetingNoteDeletedPayload{}),
	KindCallReceived:            reflect.TypeOf(CallPayload{}),
	KindCallSent:                reflect.TypeOf(CallPayload{}),
}

// IsKnownKind reports whether kind has a registered payload type. Used by
// ingestion validation (spec §3.5) to reject unknown kinds pre-tx without
// exposing the private kindPayloadTypes registry.
func IsKnownKind(kind Kind) bool {
	_, ok := kindPayloadTypes[kind]
	return ok
}

// ValidatePayload decodes env.Payload into a zero-value of env.Kind's
// canonical payload type (via reflect.New) and returns a descriptive error
// on kind-mismatch / unknown kind / decode failure. The decoded value is
// discarded — this is a validation-only helper for call sites that don't
// know the payload type P at compile time (e.g., the HTTP ingestion
// handler, which dispatches on a string-valued kind from the wire).
//
// Uses json.Unmarshal with default (lenient) settings: unknown fields are
// silently dropped. This matches spec §3.2's Version-int evolution model —
// forward-compat additions must not be rejected at the ingest boundary.
// Required-field presence is NOT checked (absent non-pointer fields decode
// to zero values); consumers enforce their own required-field contracts.
func ValidatePayload(env *Envelope) error {
	if env == nil {
		return fmt.Errorf("validate: nil envelope")
	}
	expected, ok := kindPayloadTypes[env.Kind]
	if !ok {
		return fmt.Errorf("validate: unknown kind %q", env.Kind)
	}
	if len(env.Payload) == 0 {
		return fmt.Errorf("validate %s: empty payload", env.Kind)
	}
	// Reject literal `null`: json.Unmarshal of `null` into a struct
	// pointer succeeds and leaves the struct zero-valued, which would
	// silently pass a garbage event through to the event table. Rejecting
	// here matches the ingest boundary's "payload is the kind's typed
	// struct" contract.
	if isJSONNull(env.Payload) {
		return fmt.Errorf("validate %s: payload must be an object, got null", env.Kind)
	}
	dst := reflect.New(expected).Interface() // *P
	if err := json.Unmarshal(env.Payload, dst); err != nil {
		return fmt.Errorf("validate %s: %w", env.Kind, err)
	}
	// Per-kind value checks beyond structural decode. Currently the
	// only kinds with strict value rules at the validate boundary are
	// the raw_message.* daemon-emitted kinds: their MessageType must
	// match the messages_message CHECK constraint set so a misshaped
	// payload doesn't abort the ingest tx with a constraint violation.
	switch env.Kind {
	case KindRawMessageReceived:
		p := dst.(*RawMessageReceivedPayload)
		if p.MessageType != "" && !IsCanonicalMessageType(p.MessageType) {
			return fmt.Errorf("validate %s: message_type %q is not in the canonical set", env.Kind, p.MessageType)
		}
	case KindRawMessageSent:
		p := dst.(*RawMessageSentPayload)
		if p.MessageType != "" && !IsCanonicalMessageType(p.MessageType) {
			return fmt.Errorf("validate %s: message_type %q is not in the canonical set", env.Kind, p.MessageType)
		}
	case KindExternalContactUpserted:
		p := dst.(*ExternalContactUpsertedPayload)
		if p.EntityID == "" {
			return fmt.Errorf("validate %s: entity_id is required", env.Kind)
		}
		if p.HostID == uuid.Nil {
			return fmt.Errorf("validate %s: host_id is required", env.Kind)
		}
		if p.Birthday != nil && *p.Birthday != "" {
			if _, err := time.Parse("2006-01-02", *p.Birthday); err != nil {
				return fmt.Errorf("validate %s: birthday %q not in YYYY-MM-DD form", env.Kind, *p.Birthday)
			}
		}
	case KindExternalContactDeleted:
		p := dst.(*ExternalContactDeletedPayload)
		if p.EntityID == "" {
			return fmt.Errorf("validate %s: entity_id is required", env.Kind)
		}
		if p.HostID == uuid.Nil {
			return fmt.Errorf("validate %s: host_id is required", env.Kind)
		}
	case KindMeetingNoteRecorded:
		p := dst.(*MeetingNoteRecordedPayload)
		if p.HostID == uuid.Nil {
			return fmt.Errorf("validate %s: host_id is required", env.Kind)
		}
		if _, err := uuid.Parse(p.SourceID); err != nil {
			return fmt.Errorf("validate %s: source_id must be a UUID: %w", env.Kind, err)
		}
		if p.MeetingAt.IsZero() {
			return fmt.Errorf("validate %s: meeting_at is required", env.Kind)
		}
		for i, pid := range p.ParticipantIDs {
			if _, err := uuid.Parse(pid); err != nil {
				return fmt.Errorf("validate %s: participant_ids[%d] not a UUID: %w", env.Kind, i, err)
			}
		}
	case KindMeetingNoteDeleted:
		p := dst.(*MeetingNoteDeletedPayload)
		if p.HostID == uuid.Nil {
			return fmt.Errorf("validate %s: host_id is required", env.Kind)
		}
		if _, err := uuid.Parse(p.SourceID); err != nil {
			return fmt.Errorf("validate %s: source_id must be a UUID: %w", env.Kind, err)
		}
	case KindCallReceived, KindCallSent:
		p := dst.(*CallPayload)
		if err := validateCallPayload(env.Kind, p); err != nil {
			return err
		}
	}
	return nil
}

// validateCallPayload enforces the per-kind invariants for CallPayload:
// service enum membership, direction-vs-kind agreement, non-empty
// call_unique_id and peer_handle/peer_normalized, non-negative duration.
// Cross-checks are conservative — the inline ingest handler runs further
// invariants (re-canonicalize peer, identity match), but rejecting at the
// payload boundary keeps malformed events out of the event-log entirely.
func validateCallPayload(kind Kind, p *CallPayload) error {
	if p.HostID == uuid.Nil {
		return fmt.Errorf("validate %s: host_id is required", kind)
	}
	if p.CallUniqueID == "" {
		return fmt.Errorf("validate %s: call_unique_id is required", kind)
	}
	if p.PeerHandle == "" {
		return fmt.Errorf("validate %s: peer_handle is required", kind)
	}
	if p.PeerNormalized == "" {
		return fmt.Errorf("validate %s: peer_normalized is required", kind)
	}
	if p.Service == "" || !IsCanonicalCallService(p.Service) {
		return fmt.Errorf("validate %s: service %q is not in the canonical set", kind, p.Service)
	}
	switch kind {
	case KindCallReceived:
		if p.Direction != "" && p.Direction != "inbound" {
			return fmt.Errorf("validate %s: direction must be \"inbound\" or empty, got %q", kind, p.Direction)
		}
	case KindCallSent:
		if p.Direction != "" && p.Direction != "outbound" {
			return fmt.Errorf("validate %s: direction must be \"outbound\" or empty, got %q", kind, p.Direction)
		}
	}
	if p.DurationSeconds < 0 {
		return fmt.Errorf("validate %s: duration_seconds must be non-negative, got %d", kind, p.DurationSeconds)
	}
	if p.StartedAt.IsZero() {
		return fmt.Errorf("validate %s: started_at is required", kind)
	}
	return nil
}

// isJSONNull reports whether b is the literal JSON token `null`, ignoring
// surrounding whitespace. Used to distinguish an absent/null payload from
// a real object payload at the ingestion boundary.
func isJSONNull(b []byte) bool {
	lo, hi := 0, len(b)
	for lo < hi && isJSONSpace(b[lo]) {
		lo++
	}
	for hi > lo && isJSONSpace(b[hi-1]) {
		hi--
	}
	trimmed := b[lo:hi]
	return len(trimmed) == 4 &&
		trimmed[0] == 'n' && trimmed[1] == 'u' &&
		trimmed[2] == 'l' && trimmed[3] == 'l'
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// Marshal serializes a typed payload for an Envelope's Payload field and
// asserts the payload's type matches the canonical type for kind. Callers
// should populate payload.Version before calling.
func Marshal[P any](kind Kind, payload P) (json.RawMessage, error) {
	expected, ok := kindPayloadTypes[kind]
	if !ok {
		return nil, fmt.Errorf("marshal: unknown kind %q", kind)
	}
	got := reflect.TypeOf(payload)
	if got != expected {
		return nil, fmt.Errorf("marshal: kind %s expects payload %s, got %s",
			kind, expected, got)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", kind, err)
	}
	return json.RawMessage(b), nil
}

// Unmarshal deserializes an Envelope's Payload into dst and asserts that
// env.Kind's canonical payload type matches P. Callers should inspect
// dst.Version after decode to branch on future V2+ structs. Returns an
// error for: nil envelope, nil dst, empty payload, unknown kind,
// kind-vs-type mismatch, or JSON decode failure.
func Unmarshal[P any](env *Envelope, dst *P) error {
	if env == nil {
		return fmt.Errorf("unmarshal: nil envelope")
	}
	if dst == nil {
		return fmt.Errorf("unmarshal %s: nil dst", env.Kind)
	}
	if len(env.Payload) == 0 {
		return fmt.Errorf("unmarshal %s: empty payload", env.Kind)
	}
	expected, ok := kindPayloadTypes[env.Kind]
	if !ok {
		return fmt.Errorf("unmarshal: unknown kind %q", env.Kind)
	}
	got := reflect.TypeOf(*dst)
	if got != expected {
		return fmt.Errorf("unmarshal: envelope kind %s decodes to %s, not %s",
			env.Kind, expected, got)
	}
	if err := json.Unmarshal(env.Payload, dst); err != nil {
		return fmt.Errorf("unmarshal %s: %w", env.Kind, err)
	}
	return nil
}
