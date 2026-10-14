package handlers

import "time"

// InteractionResponse represents an interaction in API responses.
type InteractionResponse struct {
	ID          string    `json:"id"`
	ContactID   string    `json:"contact_id"`
	Source      string    `json:"source"`
	SourceRef   *string   `json:"source_ref,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
	Description *string   `json:"description,omitempty"`
	Direction   string    `json:"direction" tstype:"InteractionDirection"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateInteractionRequest represents the request to create an interaction.
type CreateInteractionRequest struct {
	OccurredAt  *string `json:"occurred_at,omitempty"`
	Description *string `json:"description,omitempty"`
	Direction   *string `json:"direction,omitempty" tstype:"InteractionDirection"`
}

type VenueTagResponse struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Kind    string `json:"kind" tstype:"InteractionVenueKind"`
	IsGroup bool   `json:"is_group"`
}

type EventSummaryResponse struct {
	Title         *string   `json:"title,omitempty"`
	Location      *string   `json:"location,omitempty"`
	AttendeeCount int       `json:"attendee_count"`
	StartTime     time.Time `json:"start_time"`
	EndTime       time.Time `json:"end_time"`
	HTMLLink      *string   `json:"html_link,omitempty"`
}

type CallSummaryResponse struct {
	Service         string `json:"service" tstype:"CallService"`
	Direction       string `json:"direction" tstype:"CallDirection"`
	Answered        *bool  `json:"answered,omitempty"`
	HasVoicemail    bool   `json:"has_voicemail"`
	DurationSeconds int    `json:"duration_seconds"`
}

// InteractionListItemResponse is one item of a contact's interaction history.
// Kind "interaction" is a recorded interaction and ID its interaction id; kind
// "missed_call" is a missed inbound call that left no voicemail, ID the call
// record's id. A missed call bears no cadence, which CadenceInert states.
type InteractionListItemResponse struct {
	Kind         string                `json:"kind" tstype:"TimelineItemKind"`
	CadenceInert bool                  `json:"cadence_inert"`
	ID           string                `json:"id"`
	ContactID    string                `json:"contact_id"`
	Source       string                `json:"source"`
	SourceRef    *string               `json:"source_ref,omitempty"`
	OccurredAt   time.Time             `json:"occurred_at"`
	Description  *string               `json:"description,omitempty"`
	Direction    string                `json:"direction" tstype:"InteractionDirection"`
	CreatedAt    time.Time             `json:"created_at"`
	Label        string                `json:"label"`
	ContentKind  string                `json:"content_kind" tstype:"InteractionContentKind"`
	MessageCount int                   `json:"message_count"`
	IsGroup      bool                  `json:"is_group"`
	VenueTags    []VenueTagResponse    `json:"venue_tags"`
	Event        *EventSummaryResponse `json:"event,omitempty"`
	Call         *CallSummaryResponse  `json:"call,omitempty"`
}

type InteractionListResponse struct {
	Items        []InteractionListItemResponse `json:"items"`
	VenueOptions []VenueTagResponse            `json:"venue_options"`
}

type InteractionContentResponse struct {
	InteractionID string                       `json:"interaction_id"`
	Kind          string                       `json:"kind" tstype:"InteractionContentKind"`
	Messages      []ContentMessageResponse     `json:"messages"`
	MeetingNotes  []MeetingNoteContentResponse `json:"meeting_notes"`
}

type ContentMessageResponse struct {
	ID         string    `json:"id"`
	Sender     string    `json:"sender"`
	IsOutgoing bool      `json:"is_outgoing"`
	SentAt     time.Time `json:"sent_at"`
	Body       string    `json:"body"`
	VenueKey   string    `json:"venue_key"`
}

type MeetingNoteContentResponse struct {
	Title   *string `json:"title,omitempty"`
	Summary *string `json:"summary,omitempty"`
	Memo    *string `json:"memo,omitempty"`
}

// InteractionMergeRequest declares the listed interactions one interaction for
// every attendee. ContactID names the contact whose resulting participation
// the merge answers with.
type InteractionMergeRequest struct {
	InteractionIDs []string `json:"interaction_ids" binding:"required"`
	ContactID      string   `json:"contact_id" binding:"required"`
}

// InteractionParticipationResponse is one contact's effective participation in
// an interaction.
type InteractionParticipationResponse struct {
	Direction  string    `json:"direction" tstype:"InteractionDirection"`
	OccurredAt time.Time `json:"occurred_at"`
}

// InteractionMergeResponse answers a merge or its preview: the merged
// interaction (the root of the first listed id) and the requesting contact's
// participation in it.
type InteractionMergeResponse struct {
	InteractionID string                           `json:"interaction_id"`
	Participation InteractionParticipationResponse `json:"participation"`
}

// InteractionRemovalScope selects whose participations a removal removes:
// the listed contacts, or everyone. Exactly one is given; the fields are
// pointers so a scope's presence is read from the body, not its zero value.
type InteractionRemovalScope struct {
	ContactIDs *[]string `json:"contact_ids,omitempty"`
	Everyone   *bool     `json:"everyone,omitempty"`
}

// InteractionRemovalRequest removes participations of an interaction. Reason
// explains the removal and never changes what is removed.
type InteractionRemovalRequest struct {
	Scope  InteractionRemovalScope `json:"scope"`
	Reason string                  `json:"reason" tstype:"'duplicate' | 'did_not_attend' | 'did_not_happen'"`
}
