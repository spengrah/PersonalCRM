// Package repository — LinkageCandidate is the sum-type-shaped value
// returned by linkage-detection queries. The kind tag discriminates the
// rest of the fields: "event" candidates come from calendar_event;
// "phone_call" candidates come from phone_call.
package repository

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// LinkageCandidate is a normalized candidate row used by the
// meeting_note.recorded inline handler's linkage-detection algorithm.
//
// Fields by kind:
//   - "event":      AttendeeContactIDs populated (matched_contact_ids);
//     PeerContactID nil.
//   - "phone_call": PeerContactID populated (may be nil when the daemon
//     failed to resolve the peer); AttendeeContactIDs nil.
type LinkageCandidate struct {
	Kind               string
	ID                 uuid.UUID
	OccurredAt         time.Time
	AttendeeContactIDs []uuid.UUID
	PeerContactID      *uuid.UUID

	// Coalescing inputs — read only by the service-layer
	// coalesceCandidates pass that collapses semantically-identical
	// candidates within a kind before linkage classification. Populated
	// by the two FindLinkageCandidatesTx projections from data they
	// already read; left zero on any other construction site (which never
	// feeds coalesceCandidates). Event candidates carry NormalizedTitle;
	// phone_call candidates carry the rest.
	NormalizedTitle string     // event: NormalizeCoalesceTitle(title); phone_call: ""
	PeerNormalized  string     // phone_call: normalized peer number; event: ""
	Service         string     // phone_call: voice/facetime_audio/...; event: ""
	Direction       string     // phone_call: inbound/outbound; event: ""
	DurationSeconds int32      // phone_call: call duration; event: 0
	Answered        *bool      // phone_call: three-state (nil outbound / true / false); event: nil
	InteractionID   *uuid.UUID // phone_call: linked interaction, if any; event: nil
}

// NormalizeCoalesceTitle normalizes a calendar event title for the
// service-layer coalescing pass's same-meeting group key. It lowercases
// and collapses internal/leading/trailing whitespace; a nil or
// whitespace-only title normalizes to "". Exact normalized equality only
// (no fuzzy matching), matching the "same normalized title" rule. Shared
// between the calendar projection and the service-layer coalescer so both
// derive identical keys.
func NormalizeCoalesceTitle(title *string) string {
	if title == nil {
		return ""
	}
	return strings.ToLower(strings.Join(strings.Fields(*title), " "))
}

// ImpliedAttendeeSet returns the candidate's intrinsic participant
// contact-ID set, used by Step 5's walk-in supplemental rule. For an
// event candidate, the set is built from AttendeeContactIDs; for a
// phone_call candidate, it contains PeerContactID (or is empty when
// the peer is unresolved). Making this explicit prevents a false-
// walk-in bug on phone_call links — a tagged contact that IS the peer
// must NOT be added as a walk-in, but a tagged contact who isn't the
// peer must be.
func (c LinkageCandidate) ImpliedAttendeeSet() map[uuid.UUID]struct{} {
	switch c.Kind {
	case LinkedKindEvent:
		set := make(map[uuid.UUID]struct{}, len(c.AttendeeContactIDs))
		for _, aid := range c.AttendeeContactIDs {
			set[aid] = struct{}{}
		}
		return set
	case LinkedKindPhoneCall:
		set := make(map[uuid.UUID]struct{}, 1)
		if c.PeerContactID != nil {
			set[*c.PeerContactID] = struct{}{}
		}
		return set
	default:
		return map[uuid.UUID]struct{}{}
	}
}

// ConflictCandidateSummary is the persisted JSONB snapshot entry for a
// single candidate at the moment linkage_state was set to
// conflict_pending. The full snapshot — a sorted array of these — lands
// on meeting_note.conflict_candidates so that the resolve-link endpoint
// can validate user-supplied (kind, id) tuples and the needs-attention
// list endpoint can project candidate previews without recomputing the
// candidate set against a moving target.
//
// Sort order: overlap_count desc, then occurred_at asc as a deterministic
// tie-breaker (Go map iteration order is randomized; a stable snapshot
// reduces re-sync diff noise).
type ConflictCandidateSummary struct {
	Kind         string    `json:"kind"`
	ID           uuid.UUID `json:"id"`
	OccurredAt   time.Time `json:"occurred_at"`
	OverlapCount int       `json:"overlap_count"`
}
