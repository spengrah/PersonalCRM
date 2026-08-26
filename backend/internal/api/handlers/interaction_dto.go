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
	Answered        *bool  `json:"answered,omitempty"`
	HasVoicemail    bool   `json:"has_voicemail"`
	DurationSeconds int    `json:"duration_seconds"`
}

type InteractionListItemResponse struct {
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
