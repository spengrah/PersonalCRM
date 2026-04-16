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
// distinguishes cadence vs action task completion semantics (per spec
// §3.4.5 publisher hooks).
type TaskCompletedPayload struct {
	Version     int       `json:"version"`
	ContactID   uuid.UUID `json:"contact_id"`
	TaskID      string    `json:"task_id"`   // external provider id (e.g., todoist)
	TaskKind    string    `json:"task_kind"` // "cadence" | "follow_up" | "action"
	CompletedAt time.Time `json:"completed_at"`
	Direction   string    `json:"direction"` // "outbound" | "mutual"
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
}

// CadenceFieldsSnapshot is the four-cadence-column snapshot embedded in
// InteractionRecordedPayload (V2+). See plan Decision 2a.
type CadenceFieldsSnapshot struct {
	LastContacted  *time.Time `json:"last_contacted,omitempty"`
	LastOutreachAt *time.Time `json:"last_outreach_at,omitempty"`
	LastResponseAt *time.Time `json:"last_response_at,omitempty"`
	ContactBy      *time.Time `json:"contact_by,omitempty"` // date-precision
}

// kindPayloadTypes is the canonical Kind → payload-type registry used by
// Marshal and Unmarshal to assert type-vs-kind consistency at runtime. Add
// a row each time a new Kind + payload struct are introduced. Keep in sync
// with AllKinds (the unit test TestKindPayloadTypes_CoversAllKinds enforces
// this invariant).
var kindPayloadTypes = map[Kind]reflect.Type{
	KindMessageReceived:      reflect.TypeOf(MessageReceivedPayload{}),
	KindMessageSent:          reflect.TypeOf(MessageSentPayload{}),
	KindCalendarAttended:     reflect.TypeOf(CalendarAttendedPayload{}),
	KindCalendarDeclined:     reflect.TypeOf(CalendarDeclinedPayload{}),
	KindTaskCompleted:        reflect.TypeOf(TaskCompletedPayload{}),
	KindTaskSkipped:          reflect.TypeOf(TaskSkippedPayload{}),
	KindTaskOutreachDetected: reflect.TypeOf(TaskOutreachDetectedPayload{}),
	KindInteractionManual:    reflect.TypeOf(InteractionManualPayload{}),
	KindContactMethodsAdded:  reflect.TypeOf(ContactMethodsAddedPayload{}),
	KindInteractionRecorded:  reflect.TypeOf(InteractionRecordedPayload{}),
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
