package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// JudgmentSeedHandler records decision-model runs through the judgment store's
// write seam (S2), so browser tests can review judgments while no method runs.
// Everything it records goes through the writers a run uses, so a fixture holds
// only records a run could write. Test-only: registered under CRM_ENV=testing.
type JudgmentSeedHandler struct {
	store        *judgment.Store
	interactions *repository.InteractionRepository
}

// NewJudgmentSeedHandler creates the test-only judgment seeding handler.
func NewJudgmentSeedHandler(store *judgment.Store, interactions *repository.InteractionRepository) *JudgmentSeedHandler {
	return &JudgmentSeedHandler{store: store, interactions: interactions}
}

// RegisterJudgmentTestRoutes wires the test-only judgment routes onto a group
// whose middleware already enforces the global API key:
//
//   - POST /api/v1/test/judgment-runs
//   - GET  /api/v1/test/judgment-obligations
//
// Caller gates the call on CRM_ENV in {testing, test}.
func RegisterJudgmentTestRoutes(v1 *gin.RouterGroup, handler *JudgmentSeedHandler) {
	v1.POST("/test/judgment-runs", handler.SeedRun)
	v1.GET("/test/judgment-obligations", handler.ListObligations)
}

// SeededObligationResponse is one obligation on a thread, open or closed:
// who owes (user or contact), on what basis (reply or commitment), its task's
// kind and due date (YYYY-MM-DD).
type SeededObligationResponse struct {
	ContactID string `json:"contact_id"`
	Debtor    string `json:"debtor"`
	Basis     string `json:"basis"`
	Kind      string `json:"kind"`
	DueDate   string `json:"due_date"`
	Open      bool   `json:"open"`
}

// ListObligations returns the obligations on the thread an interaction was
// recorded in, so a browser test can observe what a review opened.
// @Summary List a thread's obligations
// @Description Every obligation on the thread of thread_interaction_id, open or closed, oldest first. Test-only (CRM_ENV=testing).
// @Tags test
// @Produce json
// @Param thread_interaction_id query string true "An interaction of the thread" format(uuid)
// @Success 200 {object} api.APIResponse{data=[]SeededObligationResponse}
// @Failure 400 {object} api.APIResponse
// @Router /test/judgment-obligations [get]
func (h *JudgmentSeedHandler) ListObligations(c *gin.Context) {
	thread, err := h.threadOf(c, c.Query("thread_interaction_id"))
	if err != nil {
		api.SendValidationError(c, "Invalid thread", err.Error())
		return
	}
	obligations, err := h.store.Obligations(c.Request.Context(), thread)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	out := make([]SeededObligationResponse, len(obligations))
	for i, o := range obligations {
		out[i] = SeededObligationResponse{
			ContactID: o.ContactID.String(), Debtor: o.Debtor, Basis: o.Basis, Kind: o.Kind,
			DueDate: o.DueDate.Format("2006-01-02"), Open: o.Open,
		}
	}
	api.SendSuccess(c, http.StatusOK, out, nil)
}

// SeedJudgmentRunRequest is one run to record: its method, kind and window,
// the judgments it records (an identity the store already holds is reused),
// the threads it reports incomplete and the new messages no judgment of it
// answered. Unfinished leaves the run running.
type SeedJudgmentRunRequest struct {
	Kind           string                  `json:"kind" binding:"required"`
	MethodKey      string                  `json:"method_key" binding:"required"`
	MethodVersion  string                  `json:"method_version" binding:"required"`
	WordingVersion string                  `json:"wording_version"`
	From           *time.Time              `json:"from,omitempty"`
	To             *time.Time              `json:"to,omitempty"`
	Judgments      []SeedJudgmentRequest   `json:"judgments"`
	Incomplete     []SeedIncompleteRequest `json:"incomplete"`
	Unjudged       []SeedUnjudgedRequest   `json:"unjudged"`
	Unfinished     bool                    `json:"unfinished"`
}

// SeedJudgmentRequest is one judgment of the thread an interaction was
// recorded in. InputHash defaults to one derived from the thread and cut-off.
// A judgment that reads a window names its first message (WindowStart), the
// messages new at its cut-off (NewMessages, message keys in thread order), one
// flag decision per new message and kind (Flags), and the earlier commitments
// it re-checked (Clearings); it holds ball-in-court outcomes only. A judgment
// that names no window start reads its whole context, as a version before
// migration 099 did.
type SeedJudgmentRequest struct {
	ThreadInteractionID string                `json:"thread_interaction_id" binding:"required"`
	CutOff              time.Time             `json:"cut_off" binding:"required"`
	InputHash           string                `json:"input_hash"`
	ContextKind         string                `json:"context_kind" binding:"required"`
	WindowStart         string                `json:"window_start"`
	NewMessages         []string              `json:"new_messages"`
	Flags               []SeedFlagRequest     `json:"flags"`
	Clearings           []SeedClearingRequest `json:"clearings"`
	Questions           []SeedQuestionRequest `json:"questions"`
	BallInCourt         []SeedOutcomeRequest  `json:"ball_in_court"`
	Commitments         []SeedOutcomeRequest  `json:"commitments"`
	BigMoment           *SeedOutcomeRequest   `json:"big_moment,omitempty"`
}

// SeedFlagRequest is a flag decision: the probability that a new message holds
// a flag kind (commitment, big_moment or claim).
type SeedFlagRequest struct {
	MessageKey  string  `json:"message_key"`
	Kind        string  `json:"kind"`
	Probability float64 `json:"probability"`
}

// SeedClearingRequest is a clearing decision: whether an earlier commitment is
// done as of the judgment's cut-off. The commitment is the message flagged as a
// commitment by the run's method version at CommitmentCutOff, an earlier cut-off
// of the thread.
type SeedClearingRequest struct {
	CommitmentMessageKey string    `json:"commitment_message_key"`
	CommitmentCutOff     time.Time `json:"commitment_cut_off"`
	Done                 bool      `json:"done"`
	Probability          float64   `json:"probability"`
}

// SeedUnjudgedRequest is a new message no judgment of the run answered at a
// cut-off, and why (call_failed, message_over_limit or before_window).
type SeedUnjudgedRequest struct {
	ThreadInteractionID string    `json:"thread_interaction_id" binding:"required"`
	CutOff              time.Time `json:"cut_off" binding:"required"`
	MessageKey          string    `json:"message_key" binding:"required"`
	Reason              string    `json:"reason" binding:"required"`
}

// SeedQuestionRequest is one answered question as the method answered it.
type SeedQuestionRequest struct {
	ID             string             `json:"id"`
	Probabilities  map[string]float64 `json:"probabilities"`
	OriginalAnswer string             `json:"original_answer"`
}

// SeedOutcomeRequest is one outcome. Direction (mine or theirs) and DueDate
// (YYYY-MM-DD) apply to commitments only; ContactID to every question but the
// big moment.
type SeedOutcomeRequest struct {
	QuestionID string  `json:"question_id"`
	ContactID  string  `json:"contact_id"`
	Direction  string  `json:"direction"`
	Outcome    string  `json:"outcome"`
	Confidence float64 `json:"confidence"`
	DueDate    *string `json:"due_date,omitempty"`
}

// SeedIncompleteRequest is a thread the run reports incomplete.
type SeedIncompleteRequest struct {
	ThreadInteractionID string    `json:"thread_interaction_id" binding:"required"`
	CutOff              time.Time `json:"cut_off" binding:"required"`
	Reason              string    `json:"reason" binding:"required"`
}

// SeedJudgmentRunResponse names the recorded run and the thread of each
// judgment, in request order.
type SeedJudgmentRunResponse struct {
	RunID   string   `json:"run_id"`
	Threads []string `json:"threads"`
}

// SeedRun records one run.
// @Summary Seed a judgment run
// @Description Record a decision-model run, its judgments (with the window, new messages, flag decisions and clearing decisions a run records), its incomplete threads and its unjudged messages through the judgment store. Test-only (CRM_ENV=testing).
// @Tags test
// @Accept json
// @Produce json
// @Param body body SeedJudgmentRunRequest true "The run"
// @Success 201 {object} api.APIResponse{data=SeedJudgmentRunResponse}
// @Failure 400 {object} api.APIResponse
// @Router /test/judgment-runs [post]
func (h *JudgmentSeedHandler) SeedRun(c *gin.Context) {
	var req SeedJudgmentRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid judgment run", err.Error())
		return
	}
	ctx := c.Request.Context()
	method := judgment.MethodRef{Key: req.MethodKey, Version: req.MethodVersion, WordingVersion: req.WordingVersion}
	run, err := h.store.StartRun(ctx, judgment.RunSpec{Kind: judgment.RunKind(req.Kind), Method: method, From: req.From, To: req.To})
	if err != nil {
		api.SendValidationError(c, "Invalid judgment run", err.Error())
		return
	}
	threads := make([]string, len(req.Judgments))
	for i, jr := range req.Judgments {
		j, err := h.judgmentOf(c, method, jr)
		if err != nil {
			api.SendValidationError(c, "Invalid judgment", err.Error())
			return
		}
		if _, err := h.store.RecordJudgment(ctx, run, j); err != nil {
			api.SendValidationError(c, "Invalid judgment", err.Error())
			return
		}
		threads[i] = j.Thread.String()
	}
	for _, ir := range req.Incomplete {
		thread, err := h.threadOf(c, ir.ThreadInteractionID)
		if err != nil {
			api.SendValidationError(c, "Invalid incomplete thread", err.Error())
			return
		}
		if err := h.store.RecordIncomplete(ctx, run, thread, ir.CutOff, judgment.IncompleteReason(ir.Reason)); err != nil {
			api.SendValidationError(c, "Invalid incomplete thread", err.Error())
			return
		}
	}
	if len(req.Unjudged) > 0 {
		unjudged := make([]judgment.UnjudgedMessage, len(req.Unjudged))
		for i, ur := range req.Unjudged {
			thread, err := h.threadOf(c, ur.ThreadInteractionID)
			if err != nil {
				api.SendValidationError(c, "Invalid unjudged message", err.Error())
				return
			}
			unjudged[i] = judgment.UnjudgedMessage{Thread: thread, CutOff: ur.CutOff, MessageKey: ur.MessageKey, Reason: judgment.UnjudgedReason(ur.Reason)}
		}
		if err := h.store.RecordUnjudged(ctx, run, unjudged); err != nil {
			api.SendValidationError(c, "Invalid unjudged message", err.Error())
			return
		}
	}
	if !req.Unfinished {
		if err := h.store.FinishRun(ctx, run); err != nil {
			api.RespondInternal(c, err)
			return
		}
	}
	api.SendSuccess(c, http.StatusCreated, SeedJudgmentRunResponse{RunID: run.String(), Threads: threads}, nil)
}

// threadOf is the venue an interaction was recorded in: the interaction an
// observation ID names, or the first observation of the interaction a root
// interaction node ID names.
func (h *JudgmentSeedHandler) threadOf(c *gin.Context, interactionID string) (uuid.UUID, error) {
	id, err := uuid.Parse(interactionID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("thread_interaction_id must be a UUID")
	}
	ctx := c.Request.Context()
	ix, err := h.interactions.GetInteraction(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		trees, treeErr := h.interactions.ListInteractionTreeObservations(ctx, []uuid.UUID{id})
		if treeErr != nil {
			return uuid.Nil, treeErr
		}
		if len(trees[id]) == 0 {
			return uuid.Nil, fmt.Errorf("interaction %s not found", id)
		}
		ix, err = &trees[id][0], nil
	}
	if err != nil {
		return uuid.Nil, err
	}
	if ix.VenueID == nil {
		return uuid.Nil, fmt.Errorf("interaction %s was recorded in no venue", id)
	}
	return *ix.VenueID, nil
}

func (h *JudgmentSeedHandler) judgmentOf(c *gin.Context, method judgment.MethodRef, r SeedJudgmentRequest) (judgment.Judgment, error) {
	thread, err := h.threadOf(c, r.ThreadInteractionID)
	if err != nil {
		return judgment.Judgment{}, err
	}
	hash := r.InputHash
	if hash == "" {
		hash = "seed:" + thread.String() + ":" + r.CutOff.UTC().Format(time.RFC3339Nano)
	}
	j := judgment.Judgment{
		Method: method, Thread: thread, Kind: threadctx.ContextKind(r.ContextKind),
		CutOff: r.CutOff, JudgedAt: r.CutOff, InputHash: hash,
		Usage:       judgment.Usage{Questions: len(r.Questions)},
		WindowStart: r.WindowStart, NewMessages: r.NewMessages,
	}
	for _, f := range r.Flags {
		j.Flags = append(j.Flags, judgment.FlagDecision{MessageKey: f.MessageKey, Kind: judgment.FlagKind(f.Kind), Probability: f.Probability})
	}
	if len(r.Clearings) > 0 {
		earlier, err := h.store.JudgmentsThrough(c.Request.Context(), method, thread, r.CutOff)
		if err != nil {
			return judgment.Judgment{}, err
		}
		for _, cr := range r.Clearings {
			at := slices.IndexFunc(earlier, func(e judgment.EarlierJudgment) bool {
				return e.CutOff.Equal(cr.CommitmentCutOff) && slices.ContainsFunc(e.Flags, func(f judgment.FlagDecision) bool {
					return f.MessageKey == cr.CommitmentMessageKey && f.Kind == judgment.FlagKindCommitment
				})
			})
			if at < 0 {
				return judgment.Judgment{}, fmt.Errorf("no commitment flag decision of message %q at %s", cr.CommitmentMessageKey, cr.CommitmentCutOff.Format(time.RFC3339))
			}
			j.Clearings = append(j.Clearings, judgment.ClearingDecision{
				CommitmentMessageKey: cr.CommitmentMessageKey, CommitmentJudgment: earlier[at].ID, Done: cr.Done, Probability: cr.Probability,
			})
		}
	}
	for _, q := range r.Questions {
		j.Questions = append(j.Questions, judgment.AnsweredQuestion{ID: q.ID, Probabilities: q.Probabilities, OriginalAnswer: q.OriginalAnswer})
	}
	for _, o := range r.BallInCourt {
		contact, err := uuid.Parse(o.ContactID)
		if err != nil {
			return judgment.Judgment{}, fmt.Errorf("ball_in_court contact_id must be a UUID")
		}
		j.Outcomes.BallInCourt = append(j.Outcomes.BallInCourt, judgment.ParticipantOutcome[judgment.BallInCourt]{
			QuestionID: o.QuestionID, ContactID: contact, Outcome: judgment.BallInCourt(o.Outcome), Confidence: o.Confidence,
		})
	}
	for _, o := range r.Commitments {
		contact, err := uuid.Parse(o.ContactID)
		if err != nil {
			return judgment.Judgment{}, fmt.Errorf("commitment contact_id must be a UUID")
		}
		var due *time.Time
		if o.DueDate != nil {
			d, err := time.Parse("2006-01-02", *o.DueDate)
			if err != nil {
				return judgment.Judgment{}, fmt.Errorf("commitment due_date must be YYYY-MM-DD")
			}
			due = &d
		}
		j.Outcomes.Commitments = append(j.Outcomes.Commitments, judgment.CommitmentOutcome{
			QuestionID: o.QuestionID, ContactID: contact, Direction: judgment.CommitmentDirection(o.Direction),
			Outcome: judgment.CommitmentState(o.Outcome), Confidence: o.Confidence, DueDate: due,
		})
	}
	if o := r.BigMoment; o != nil {
		j.Outcomes.BigMoment = judgment.Scored[judgment.BigMoment]{QuestionID: o.QuestionID, Outcome: judgment.BigMoment(o.Outcome), Confidence: o.Confidence}
	}
	return j, nil
}
