package handlers

import "time"

// JudgmentRunsResponse lists every decision-model run.
type JudgmentRunsResponse struct {
	Runs []JudgmentRunResponse `json:"runs"`
}

// JudgmentRunResponse is one run: its kind, method, window, start and finish,
// the judgments it holds (reused ones included) and the threads it could not
// judge.
type JudgmentRunResponse struct {
	ID            string                     `json:"id"`
	Kind          string                     `json:"kind" tstype:"RunKind"`
	MethodKey     string                     `json:"method_key"`
	MethodVersion string                     `json:"method_version"`
	From          *time.Time                 `json:"from,omitempty"`
	To            *time.Time                 `json:"to,omitempty"`
	StartedAt     time.Time                  `json:"started_at"`
	FinishedAt    *time.Time                 `json:"finished_at,omitempty"`
	JudgedCount   int                        `json:"judged_count"`
	Incomplete    []IncompleteThreadResponse `json:"incomplete"`
}

// IncompleteThreadResponse is a thread a run reported incomplete, named by
// its venue's kind and title (absent when the venue has none).
type IncompleteThreadResponse struct {
	ThreadID  string    `json:"thread_id"`
	CutOff    time.Time `json:"cut_off"`
	Reason    string    `json:"reason" tstype:"IncompleteReason"`
	VenueKind string    `json:"venue_kind"`
	Title     *string   `json:"title,omitempty"`
}

// JudgmentsResponse lists judgments of a thread, a run or both.
type JudgmentsResponse struct {
	Judgments []JudgmentResponse `json:"judgments"`
}

// JudgmentResponse is one judgment. cut_off is its first appearance's, as of
// which its content is shown; runs lists every run it appears in, each at that
// run's cut-off.
type JudgmentResponse struct {
	ID            string                          `json:"id"`
	ThreadID      string                          `json:"thread_id"`
	CutOff        time.Time                       `json:"cut_off"`
	Runs          []JudgmentRunAppearanceResponse `json:"runs"`
	MethodKey     string                          `json:"method_key"`
	MethodVersion string                          `json:"method_version"`
	ContextKind   string                          `json:"context_kind"`
	Outcomes      []JudgmentOutcomeResponse       `json:"outcomes"`
}

// JudgmentRunAppearanceResponse is a run a judgment appears in.
type JudgmentRunAppearanceResponse struct {
	RunID  string    `json:"run_id"`
	CutOff time.Time `json:"cut_off"`
}

// JudgmentOutcomeResponse is one outcome with its state and the user's
// review. contact_id is absent for big_moment; due_date is a commitment's
// selected date (YYYY-MM-DD).
type JudgmentOutcomeResponse struct {
	ID         string                         `json:"id"`
	Question   string                         `json:"question" tstype:"Question"`
	ContactID  *string                        `json:"contact_id,omitempty"`
	Outcome    string                         `json:"outcome" tstype:"BallInCourt | CommitmentState | BigMoment"`
	Confidence float64                        `json:"confidence"`
	DueDate    *string                        `json:"due_date,omitempty"`
	State      string                         `json:"state" tstype:"OutcomeState"`
	Review     *JudgmentOutcomeReviewResponse `json:"review,omitempty"`
}

// JudgmentOutcomeReviewResponse is the user's review: the right outcome the
// user stated and the verdict the server derived from it.
type JudgmentOutcomeReviewResponse struct {
	Verdict      string    `json:"verdict" tstype:"Verdict"`
	RightOutcome string    `json:"right_outcome"`
	ReviewedAt   time.Time `json:"reviewed_at"`
}

// ReviewJudgmentOutcomeRequest is a review: the outcome's right outcome. The
// server derives the verdict; a client never sends one. keep_current_date,
// allowed only with an open commitment's right outcome, keeps the
// obligation's current date against the judged later one. rows names the
// rows the bench shows that the review can change.
type ReviewJudgmentOutcomeRequest struct {
	RightOutcome    string            `json:"right_outcome" binding:"required"`
	KeepCurrentDate bool              `json:"keep_current_date,omitempty"`
	Rows            []BenchRowRequest `json:"rows,omitempty"`
}

// ReviewJudgmentOutcomeResponse is a recorded review: the reviewed outcome,
// and the rows the request named as the bench now shows them, so the bench
// shows the recorded state without reading its whole list again.
type ReviewJudgmentOutcomeResponse struct {
	JudgmentOutcomeResponse `tstype:",extends"`
	Rows                    []BenchRowStateResponse `json:"rows"`
}

// BenchRowRequest names a row the bench shows by the judgments of the
// proposals it shows.
type BenchRowRequest struct {
	JudgmentIDs []string `json:"judgment_ids"`
}

// BenchRowsRequest names the rows the bench shows that a save can change.
type BenchRowsRequest struct {
	Rows []BenchRowRequest `json:"rows,omitempty"`
}

// BenchRowStateResponse is a row as the bench shows it after a save: its
// proposals, and what remains to review across them, as the list's
// to_review and review_status read.
type BenchRowStateResponse struct {
	Proposals    []BenchProposalResponse `json:"proposals"`
	ToReview     int                     `json:"to_review"`
	ReviewStatus *string                 `json:"review_status,omitempty" tstype:"ReviewStatus"`
}

// BenchRowsResponse is the rows a save's request named, in its order, as the
// bench now shows them.
type BenchRowsResponse struct {
	Rows []BenchRowStateResponse `json:"rows"`
}

// CloseBenchRowRequest is a close of a row on the bench: the judgments of the
// proposals the row shows, and the rows the bench shows that the close can
// change.
type CloseBenchRowRequest struct {
	JudgmentIDs []string          `json:"judgment_ids" binding:"required,min=1"`
	Rows        []BenchRowRequest `json:"rows,omitempty"`
}
