// Package repository — LinkageCandidate is the sum-type-shaped value
// returned by linkage-detection queries. The kind tag (currently
// "event") discriminates the rest of the fields. The phone_call kind
// will activate when the phone_call table lands (phase 1.5); the
// CalendarEventRepository.FindLinkageCandidatesTx method is the single
// touch site that needs extending at that time.
package repository

import (
	"time"

	"github.com/google/uuid"
)

// LinkageCandidate is a normalized candidate row used by the
// meeting_note.recorded inline handler's linkage-detection algorithm.
// Kind=="event" means the candidate came from calendar_event; Kind==
// "phone_call" is reserved for the future phone_call table.
//
// Fields by kind:
//   - "event":      AttendeeContactIDs populated (matched_contact_ids);
//     PeerContactID nil.
//   - "phone_call": PeerContactID populated; AttendeeContactIDs nil.
type LinkageCandidate struct {
	Kind               string
	ID                 uuid.UUID
	OccurredAt         time.Time
	AttendeeContactIDs []uuid.UUID
	PeerContactID      *uuid.UUID
}
