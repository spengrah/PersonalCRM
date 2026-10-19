package handlers

import "time"

// ThreadVenue, FlagKind, ChipState, ClearingLabelState, ThreadStatus,
// UnjudgedReason and RunKind are the enumerations the Threads responses carry,
// declared in internal/judgment/values.go, with InteractionType, declared in
// internal/service/interaction_types.go. The generated types hold each as a
// union; a field names its union in its tstype tag.

// ThreadParticipantResponse is a contact taking part in a thread.
type ThreadParticipantResponse struct {
	ContactID string `json:"contact_id"`
	Name      string `json:"name"`
}

// ThreadRowResponse is one thread of the list, and the header of a thread
// page. venue is the app the thread lives in, and type its interaction type;
// title is absent where the venue has none; latest_at is the time of its latest message and message_count its
// count of messages, a meeting's notes counted as its messages. review is
// present when the list is narrowed to a run.
type ThreadRowResponse struct {
	ThreadID     string                      `json:"thread_id"`
	Venue        string                      `json:"venue" tstype:"ThreadVenue"`
	Type         string                      `json:"type" tstype:"InteractionType"`
	IsGroup      bool                        `json:"is_group"`
	Title        *string                     `json:"title,omitempty"`
	Participants []ThreadParticipantResponse `json:"participants"`
	LatestAt     time.Time                   `json:"latest_at"`
	MessageCount int                         `json:"message_count"`
	Review       *ThreadRowReviewResponse    `json:"review,omitempty"`
}

// ThreadRowReviewResponse is what a run shows of a thread: judged_count is the
// number of new messages the run's judgments of the thread answered, never
// counting an unjudged message; status is the thread's status, and to_review
// the number of items awaiting the user while status is to_review.
type ThreadRowReviewResponse struct {
	JudgedCount int    `json:"judged_count"`
	Status      string `json:"status" tstype:"ThreadStatus"`
	ToReview    int    `json:"to_review"`
}

// ThreadRunSummaryResponse counts the threads a run judged by status.
type ThreadRunSummaryResponse struct {
	Judged         int `json:"judged"`
	ToReview       int `json:"to_review"`
	NothingFlagged int `json:"nothing_flagged"`
	Reviewed       int `json:"reviewed"`
}

// ThreadListResponse is one page of the threads list and the interaction types
// its type filter offers. summary is present when the list is narrowed to a
// run.
type ThreadListResponse struct {
	Items       []ThreadRowResponse             `json:"items"`
	TypeOptions []InteractionTypeOptionResponse `json:"type_options"`
	Summary     *ThreadRunSummaryResponse       `json:"summary,omitempty"`
}

// ReviewRunResponse is a run the user reviews: its kind, method, window and
// start.
type ReviewRunResponse struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind" tstype:"RunKind"`
	MethodKey     string     `json:"method_key"`
	MethodVersion string     `json:"method_version"`
	From          *time.Time `json:"from,omitempty"`
	To            *time.Time `json:"to,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
}

// ReviewRunsResponse lists the review runs, newest first.
type ReviewRunsResponse struct {
	Runs []ReviewRunResponse `json:"runs"`
}

// ThreadMessageResponse is one message of a thread: its key, its sender ("Me"
// for the user's own, else the contact's name), its time and its text.
type ThreadMessageResponse struct {
	MessageKey string    `json:"message_key"`
	Sender     string    `json:"sender"`
	SentAt     time.Time `json:"sent_at"`
	Text       string    `json:"text"`
}

// ThreadPositionResponse is a thread's place in the list as the filters keep
// it: index is 1-based, and previous_thread_id and next_thread_id are absent
// past either end.
type ThreadPositionResponse struct {
	Index            int     `json:"index"`
	Total            int     `json:"total"`
	PreviousThreadID *string `json:"previous_thread_id,omitempty"`
	NextThreadID     *string `json:"next_thread_id,omitempty"`
}

// ThreadReadResponse is a thread: its header, its messages once in thread
// order, its place in the list and, given a run, the run's review of it.
// position is absent when the filters keep the thread out of the list.
type ThreadReadResponse struct {
	Thread   ThreadRowResponse       `json:"thread"`
	Messages []ThreadMessageResponse `json:"messages"`
	Position *ThreadPositionResponse `json:"position,omitempty"`
	Review   *ThreadReviewResponse   `json:"review,omitempty"`
}

// ThreadReviewResponse is a thread's review state in a run, as a read, a label
// and a close return it. nights are in cut-off order. context_messages are the
// messages the first night's window read before its new messages, which sit in
// no night, and later_messages those after the run's last cut-off, as message
// keys. status is the thread's status; to_review the number of items awaiting
// the user while status is to_review.
type ThreadReviewResponse struct {
	Run             ReviewRunResponse `json:"run"`
	Status          string            `json:"status" tstype:"ThreadStatus"`
	ToReview        int               `json:"to_review"`
	JudgedCount     int               `json:"judged_count"`
	Nights          []NightResponse   `json:"nights"`
	ContextMessages []string          `json:"context_messages"`
	LaterMessages   []string          `json:"later_messages"`
}

// NightResponse is one judgment the run recorded or reused for the thread, at a
// cut-off. window_start is the message key of the first message its window
// read; new_message_count the number of messages new at its cut-off, judged or
// unjudged. messages are the messages placed in the night, in thread order.
// clearings are its clearing decisions and closest_unflagged holds one entry
// per flag kind. proposal is its judgment's reply proposal. to_review counts
// the chips, clearing decisions and reply items it shows without a label;
// confirm_nones is true when it shows none of them and holds no label;
// holds_label is true when a flag decision of one of its messages, one of its
// clearing decisions or a decision of its reply proposal has a label;
// labeled_none counts its judged messages showing no chip that hold a label,
// each of which says the message holds nothing.
type NightResponse struct {
	JudgmentID       string                     `json:"judgment_id"`
	MethodKey        string                     `json:"method_key"`
	MethodVersion    string                     `json:"method_version"`
	CutOff           time.Time                  `json:"cut_off"`
	WindowStart      string                     `json:"window_start"`
	NewMessageCount  int                        `json:"new_message_count"`
	Messages         []NightMessageResponse     `json:"messages"`
	Clearings        []NightClearingResponse    `json:"clearings"`
	ClosestUnflagged []ClosestUnflaggedResponse `json:"closest_unflagged"`
	Proposal         BenchProposalResponse      `json:"proposal"`
	ToReview         int                        `json:"to_review"`
	ConfirmNones     bool                       `json:"confirm_nones"`
	HoldsLabel       bool                       `json:"holds_label"`
	LabeledNone      int                        `json:"labeled_none"`
}

// NightMessageResponse is a message placed in a night. judged is true when the
// night's judgment answered it; an unjudged message carries the reason the run
// recorded for it, if any. chips are the flags it shows under the night's
// method, in flag kind order.
type NightMessageResponse struct {
	MessageKey string             `json:"message_key"`
	Judged     bool               `json:"judged"`
	Reason     *string            `json:"reason,omitempty" tstype:"UnjudgedReason"`
	Chips      []FlagChipResponse `json:"chips"`
}

// FlagChipResponse is a flag a message shows. probability is absent for an
// added chip. cleared_at, on a commitment chip, is the cut-off of the first
// later night of the run whose clearing decision of the commitment answered
// done.
type FlagChipResponse struct {
	Kind        string     `json:"kind" tstype:"FlagKind"`
	State       string     `json:"state" tstype:"ChipState"`
	Probability *float64   `json:"probability,omitempty"`
	ClearedAt   *time.Time `json:"cleared_at,omitempty"`
}

// NightClearingResponse is a clearing decision of a night: the method's
// answer to whether the commitment of commitment_message_key (sent at
// commitment_sent_at) is done as of cut_off, with the probability of that
// answer and the state of the user's label against it. cut_off and
// commitment_message_key identify the decision for a clearing label.
type NightClearingResponse struct {
	CommitmentMessageKey string    `json:"commitment_message_key"`
	CommitmentSentAt     time.Time `json:"commitment_sent_at"`
	CutOff               time.Time `json:"cut_off"`
	Done                 bool      `json:"done"`
	Probability          float64   `json:"probability"`
	Label                string    `json:"label" tstype:"ClearingLabelState"`
}

// ClosestUnflaggedResponse is, for a flag kind, the judged message of the night
// showing no chip of the kind whose probability for it is highest and above
// 0.05; display only. message and probability are absent when there is none.
type ClosestUnflaggedResponse struct {
	Kind        string                  `json:"kind" tstype:"FlagKind"`
	Message     *ClosestMessageResponse `json:"message,omitempty"`
	Probability *float64                `json:"probability,omitempty"`
}

// ClosestMessageResponse names the message a closest unflagged entry points to.
type ClosestMessageResponse struct {
	MessageKey string    `json:"message_key"`
	Sender     string    `json:"sender"`
	SentAt     time.Time `json:"sent_at"`
}

// FlagLabelRequest is the user's label of a flag decision: whether the message
// holds the kind.
type FlagLabelRequest struct {
	MessageKey string `json:"message_key" binding:"required"`
	Kind       string `json:"kind" binding:"required" tstype:"FlagKind"`
	Holds      *bool  `json:"holds" binding:"required"`
}

// ClearingLabelRequest is the user's label of a clearing decision: whether the
// commitment is done as of the cut-off.
type ClearingLabelRequest struct {
	CommitmentMessageKey string    `json:"commitment_message_key" binding:"required"`
	CutOff               time.Time `json:"cut_off" binding:"required"`
	Done                 *bool     `json:"done" binding:"required"`
}
