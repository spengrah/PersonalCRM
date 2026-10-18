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

// TimelineAssertionResponse is one assertion beside one content item a timeline
// item's drill-down displays, keyed by that item's content_ref. Subject is the
// subject node's label. An edge carries its object node's label in Object; a
// fact carries exactly one Value field (ValueDate as YYYY-MM-DD). Status is the
// assertion's recorded review status; its TypeScript type is generated from the
// store's status constants (repository/assertion_status.go).
type TimelineAssertionResponse struct {
	ContentRef  string   `json:"content_ref"`
	AssertionID string   `json:"assertion_id"`
	Predicate   string   `json:"predicate"`
	Subject     string   `json:"subject"`
	Object      *string  `json:"object,omitempty"`
	ValueText   *string  `json:"value_text,omitempty"`
	ValueNum    *float64 `json:"value_num,omitempty"`
	ValueDate   *string  `json:"value_date,omitempty"`
	ValueBool   *bool    `json:"value_bool,omitempty"`
	Status      string   `json:"status" tstype:"AssertionStatus"`
}

// TimelineAssertionsResponse is the assertions beside a timeline item's
// content, grouped by content item in content_ref order, each item's in
// recording order.
type TimelineAssertionsResponse struct {
	Items []TimelineAssertionResponse `json:"items"`
}

// AllInteractionsResponse is one page of the all-interactions view (the
// bench) and the interaction types its type filter offers.
type AllInteractionsResponse struct {
	Items       []AllInteractionItemResponse    `json:"items"`
	TypeOptions []InteractionTypeOptionResponse `json:"type_options"`
}

// InteractionTypeOptionResponse is one interaction type the view filters by.
type InteractionTypeOptionResponse struct {
	Key   string `json:"key" tstype:"InteractionType"`
	Label string `json:"label"`
}

// AllInteractionItemResponse is one row: one interaction across contacts,
// served as the contact history serves it in the participation of
// context_contact_id, its first participant. proposals are the thread
// judgments whose latest content the row holds; to_review counts their items
// awaiting review. review_status, present on a row carrying a proposal, is
// to_review while any of its proposals' items has no label, else
// nones_to_review while any holds a negative decision without a label, else
// all_reviewed.
type AllInteractionItemResponse struct {
	Item             InteractionListItemResponse      `json:"item"`
	ContextContactID string                           `json:"context_contact_id"`
	Participants     []InteractionParticipantResponse `json:"participants"`
	Preview          *InteractionPreviewResponse      `json:"preview,omitempty"`
	Proposals        []BenchProposalResponse          `json:"proposals"`
	ToReview         int                              `json:"to_review"`
	ReviewStatus     *string                          `json:"review_status,omitempty" tstype:"ReviewStatus"`
}

// InteractionPreviewResponse is the latest content a row shows: a message's
// sender and text, or a meeting note's text (note true, no sender).
type InteractionPreviewResponse struct {
	Sender *string `json:"sender,omitempty"`
	Text   string  `json:"text"`
	Note   bool    `json:"note"`
}

// BenchProposalResponse is one thread judgment compiled into the answers the
// user reviews. thread_id is the judged thread, whose content
// /interactions/threads/{thread_id}/content returns; cut_off is its first
// appearance, as of which its content is shown; reused_at is its appearance in
// the run the view is narrowed to, when that run reused it. reply_lists is
// false for a meeting. to_review counts its items without a label;
// review_status is to_review while it has one, else nones_to_review while it
// holds a negative decision without a label, else all_reviewed. confirm_nones
// is true exactly when it has no items and holds a negative decision without
// a label, and /judgments/{judgment_id}/confirm-nones labels them. lists has
// one entry per list it shows, in display order; a meeting's carries no reply
// lists.
type BenchProposalResponse struct {
	JudgmentID    string                  `json:"judgment_id"`
	ThreadID      string                  `json:"thread_id"`
	MethodKey     string                  `json:"method_key"`
	MethodVersion string                  `json:"method_version"`
	Production    bool                    `json:"production"`
	ContextKind   string                  `json:"context_kind"`
	CutOff        time.Time               `json:"cut_off"`
	ReusedAt      *time.Time              `json:"reused_at,omitempty"`
	ReplyLists    bool                    `json:"reply_lists"`
	Items         []BenchItemResponse     `json:"items"`
	Additions     []BenchAdditionResponse `json:"additions"`
	ToReview      int                     `json:"to_review"`
	ReviewStatus  string                  `json:"review_status" tstype:"ReviewStatus"`
	ConfirmNones  bool                    `json:"confirm_nones"`
	Lists         []BenchListResponse     `json:"lists"`
}

// BenchListResponse is one list a proposal shows. closest is the decision the
// judgment came closest to answering with the list's value among the
// decisions no item of the list represents, absent when the list has no
// candidate. nones_reviewed is true exactly when at least one negative
// decision sits behind the list and every one has a label.
type BenchListResponse struct {
	List          string                `json:"list" tstype:"ProposalList"`
	Closest       *BenchClosestResponse `json:"closest,omitempty"`
	NonesReviewed bool                  `json:"nones_reviewed"`
}

// BenchClosestResponse names a list's closest decision by the party fields an
// item of that list carries (contact for a reply list, contact and debtor for
// a commitment, neither for the big moment), with the probability of the
// list's value as a whole percent from 0 to 100.
type BenchClosestResponse struct {
	Contact *BenchPartyResponse `json:"contact,omitempty"`
	Debtor  *string             `json:"debtor,omitempty" tstype:"Debtor"`
	Percent int                 `json:"percent"`
}

// BenchThreadContentResponse is a judged thread's whole conversation, oldest
// first, each message once.
type BenchThreadContentResponse struct {
	Messages []ContentMessageResponse `json:"messages"`
}

// BenchPartyResponse is a contact a proposal names.
type BenchPartyResponse struct {
	ContactID string `json:"contact_id"`
	Name      string `json:"name"`
}

// BenchItemResponse is one item of a proposal, standing for one judged
// outcome. contact is a reply item's participant or a commitment's
// counterparty, absent for the flag; debtor says who owes a commitment. mark
// is the decision's label, from whichever proposal on the same content it was
// made in, absent until one exists. state is the item's live state, beside
// its mark where they disagree, absent only when a removal took effect.
// due_date is a commitment's date (YYYY-MM-DD), absent when it has no stated
// date; for a commitment whose label states its date, the date the label
// kept, and live_due_date the obligation's live date where it differs. acts
// are the item's review controls; on an item with a mark they are its
// re-review controls, and posting one records the label only.
type BenchItemResponse struct {
	OutcomeID   string                      `json:"outcome_id"`
	List        string                      `json:"list" tstype:"ProposalList"`
	Contact     *BenchPartyResponse         `json:"contact,omitempty"`
	Debtor      *string                     `json:"debtor,omitempty" tstype:"Debtor"`
	State       *string                     `json:"state,omitempty" tstype:"ItemState"`
	Confidence  *float64                    `json:"confidence,omitempty"`
	DueDate     *string                     `json:"due_date,omitempty"`
	Change      *BenchItemChangeResponse    `json:"change,omitempty"`
	Source      *BenchSourceMessageResponse `json:"source,omitempty"`
	Mark        *string                     `json:"mark,omitempty" tstype:"ItemMark"`
	Acts        []BenchActResponse          `json:"acts"`
	LiveDueDate *string                     `json:"live_due_date,omitempty"`
}

// BenchItemChangeResponse is a suggested change to a live item: its removal,
// or a later due_date (YYYY-MM-DD). confidence is absent for a removal no
// method answered.
type BenchItemChangeResponse struct {
	Kind       string   `json:"kind" tstype:"ChangeKind"`
	DueDate    *string  `json:"due_date,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
}

// BenchSourceMessageResponse is the message a commitment's judgment names as
// stating it. content_ref is the message's content reference, as the content
// route gives it; sender is "Me" for the user's own message.
type BenchSourceMessageResponse struct {
	ContentRef string    `json:"content_ref"`
	Sender     string    `json:"sender"`
	SentAt     time.Time `json:"sent_at"`
	Text       string    `json:"text"`
}

// BenchActResponse is one review control: the review body it posts to
// /judgment-outcomes/{outcome_id}/review.
type BenchActResponse struct {
	Act             string `json:"act" tstype:"ActKind"`
	RightOutcome    string `json:"right_outcome"`
	KeepCurrentDate bool   `json:"keep_current_date"`
}

// BenchAdditionResponse is one choice of a proposal's pickers: adding contact
// to a reply list, adding a commitment between the user and contact (debtor
// says who owes it), or adding the flag, by posting right_outcome to the
// outcome's review route. The pickers offer every participant or pair not
// already in the list whose decision has no label or is negative, labeled or
// not.
type BenchAdditionResponse struct {
	List         string              `json:"list" tstype:"ProposalList"`
	Contact      *BenchPartyResponse `json:"contact,omitempty"`
	Debtor       *string             `json:"debtor,omitempty" tstype:"Debtor"`
	OutcomeID    string              `json:"outcome_id"`
	RightOutcome string              `json:"right_outcome"`
}
