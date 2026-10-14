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

// TimelineIdentityResponse is the typed identity of a timeline item. For kind
// "interaction" the ID is the interaction's merge-root node; for "message_burst"
// the burst's observation; for "upcoming_event" the calendar event; for
// "missed_call" the call record.
type TimelineIdentityResponse struct {
	Kind string `json:"kind" tstype:"TimelineItemKind"`
	ID   string `json:"id"`
}

// TimelineMemberResponse is one member of a timeline item: what the item's
// representative observation of one merged interaction node, or the item itself,
// shows in the list.
type TimelineMemberResponse struct {
	Source       string                `json:"source"`
	SourceRef    *string               `json:"source_ref,omitempty"`
	Label        string                `json:"label"`
	ContentKind  string                `json:"content_kind" tstype:"InteractionContentKind"`
	MessageCount int                   `json:"message_count"`
	IsGroup      bool                  `json:"is_group"`
	VenueTags    []VenueTagResponse    `json:"venue_tags"`
	Event        *EventSummaryResponse `json:"event,omitempty"`
	Call         *CallSummaryResponse  `json:"call,omitempty"`
}

// InteractionListItemResponse is one item of a contact's interaction history,
// at the contact's effective participation. A missed call bears no cadence,
// which CadenceInert states.
type InteractionListItemResponse struct {
	Identity     TimelineIdentityResponse `json:"identity"`
	Direction    string                   `json:"direction" tstype:"InteractionDirection"`
	OccurredAt   time.Time                `json:"occurred_at"`
	CadenceInert bool                     `json:"cadence_inert"`
	Members      []TimelineMemberResponse `json:"members"`
}

type InteractionListResponse struct {
	Items        []InteractionListItemResponse `json:"items"`
	VenueOptions []VenueTagResponse            `json:"venue_options"`
}

// TimelineContentMemberResponse is the content of one member of a timeline item.
type TimelineContentMemberResponse struct {
	Source       string                       `json:"source"`
	Kind         string                       `json:"kind" tstype:"InteractionContentKind"`
	Messages     []ContentMessageResponse     `json:"messages"`
	MeetingNotes []MeetingNoteContentResponse `json:"meeting_notes"`
}

// TimelineContentResponse is the content of a timeline item.
type TimelineContentResponse struct {
	Identity TimelineIdentityResponse        `json:"identity"`
	Members  []TimelineContentMemberResponse `json:"members"`
}

// ContentMessageResponse is one message, attributed to its own source and
// container. Channel and Container are null for a message with no container.
// ContentRef is equal for every per-contact copy of the same source message.
type ContentMessageResponse struct {
	ID         string    `json:"id"`
	Sender     string    `json:"sender"`
	IsOutgoing bool      `json:"is_outgoing"`
	SentAt     time.Time `json:"sent_at"`
	Body       string    `json:"body"`
	VenueKey   string    `json:"venue_key"`
	Source     string    `json:"source"`
	Channel    *string   `json:"channel" tstype:"InteractionVenueKind | null,required"`
	Container  *string   `json:"container" tstype:"string | null,required"`
	ContentRef string    `json:"content_ref"`
}

// MeetingNoteContentResponse is one meeting note, attributed to its own
// container.
type MeetingNoteContentResponse struct {
	Title      *string `json:"title,omitempty"`
	Summary    *string `json:"summary,omitempty"`
	Memo       *string `json:"memo,omitempty"`
	Source     string  `json:"source"`
	Channel    string  `json:"channel" tstype:"'meeting' | 'session'"`
	Container  *string `json:"container" tstype:"string | null,required"`
	ContentRef string  `json:"content_ref"`
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

// InteractionParticipantResponse is one effective participant of an
// interaction: a contact the removal dialog lists.
type InteractionParticipantResponse struct {
	ContactID string `json:"contact_id"`
	Name      string `json:"name"`
}

// InteractionParticipantsResponse lists an interaction's effective
// participants, ordered by name.
type InteractionParticipantsResponse struct {
	Participants []InteractionParticipantResponse `json:"participants"`
}
