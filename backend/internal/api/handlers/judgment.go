package handlers

import (
	"errors"
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/judgment"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// JudgmentHandler serves the bench's judgment reads and reviews over the
// judgment store.
type JudgmentHandler struct {
	store *judgment.Store
}

// NewJudgmentHandler creates the judgment routes' handler.
func NewJudgmentHandler(store *judgment.Store) *JudgmentHandler {
	return &JudgmentHandler{store: store}
}

// ListJudgmentRuns returns every decision-model run.
// @Summary List judgment runs
// @Description Every run with its kind, method, window, judged count (reused judgments included) and the threads it reported incomplete.
// @Tags judgments
// @Produce json
// @Success 200 {object} api.APIResponse{data=JudgmentRunsResponse}
// @Router /judgment-runs [get]
func (h *JudgmentHandler) ListJudgmentRuns(c *gin.Context) {
	runs, err := h.store.ListRuns(c.Request.Context())
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	out := make([]JudgmentRunResponse, len(runs))
	for i, r := range runs {
		incomplete := make([]IncompleteThreadResponse, len(r.Incomplete))
		for j, t := range r.Incomplete {
			incomplete[j] = IncompleteThreadResponse{ThreadID: t.Thread.String(), CutOff: t.CutOff, Reason: string(t.Reason), VenueKind: t.VenueKind, Title: t.Title}
		}
		out[i] = JudgmentRunResponse{
			ID: r.ID.String(), Kind: string(r.Kind), MethodKey: r.Method.Key, MethodVersion: r.Method.Version,
			From: r.From, To: r.To, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
			JudgedCount: r.JudgedCount, Incomplete: incomplete,
		}
	}
	api.SendSuccess(c, http.StatusOK, JudgmentRunsResponse{Runs: out}, nil)
}

// ListJudgments returns the judgments of a thread, of a run (every judgment
// appearing in it, reused ones included) or both.
// @Summary List judgments
// @Description Judgments of a thread, a run or both, each with every run it appears in and each outcome's state and review. At least one of thread_id and run_id is required.
// @Tags judgments
// @Produce json
// @Param thread_id query string false "Thread (venue) ID" format(uuid)
// @Param run_id query string false "Run ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=JudgmentsResponse}
// @Failure 400 {object} api.APIResponse
// @Router /judgments [get]
func (h *JudgmentHandler) ListJudgments(c *gin.Context) {
	var query judgment.JudgmentQuery
	for _, p := range []struct {
		name string
		dst  **uuid.UUID
	}{{"thread_id", &query.Thread}, {"run_id", &query.Run}} {
		raw := c.Query(p.name)
		if raw == "" {
			continue
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			api.SendValidationError(c, "Invalid "+p.name, p.name+" must be a UUID")
			return
		}
		*p.dst = &id
	}
	views, err := h.store.ListJudgments(c.Request.Context(), query)
	if err != nil {
		if errors.Is(err, judgment.ErrUnboundedQuery) {
			api.SendValidationError(c, "Missing judgment filter", "thread_id or run_id is required")
			return
		}
		api.RespondInternal(c, err)
		return
	}
	out := make([]JudgmentResponse, len(views))
	for i, v := range views {
		runs := make([]JudgmentRunAppearanceResponse, len(v.Runs))
		for j, r := range v.Runs {
			runs[j] = JudgmentRunAppearanceResponse{RunID: r.Run.String(), CutOff: r.CutOff}
		}
		outcomes := make([]JudgmentOutcomeResponse, len(v.Outcomes))
		for j, o := range v.Outcomes {
			outcomes[j] = outcomeResponse(o)
		}
		out[i] = JudgmentResponse{
			ID: v.ID.String(), ThreadID: v.Thread.String(), CutOff: v.CutOff, Runs: runs,
			MethodKey: v.Method.Key, MethodVersion: v.Method.Version, ContextKind: string(v.Kind), Outcomes: outcomes,
		}
	}
	api.SendSuccess(c, http.StatusOK, JudgmentsResponse{Judgments: out}, nil)
}

// ReviewJudgmentOutcome records the user's review of an outcome.
// @Summary Review a judgment outcome
// @Description Records the right outcome the user states for an outcome of any state and returns the outcome with the verdict the server derives: confirmed when it equals the judged outcome, rejected when it differs and has no effect, corrected otherwise, beside the proposals of the outcome's thread as the bench now shows them, narrowed to run_id's judgments when it is given. The review is its decision's label for every proposal on the same content, and labels no other decision. A first review of an applied or claim outcome gives the thread the right outcome's effect; a review of a decision that already has a label records the label only, except an Add on a negative decision, which has the effect the same review of an unlabeled negative has. keep_current_date, with an open commitment's right outcome, keeps the obligation's current date against the judged later one, and the verdict is corrected.
// @Tags judgments
// @Accept json
// @Produce json
// @Param id path string true "Outcome ID" format(uuid)
// @Param run_id query string false "Run the bench is narrowed to" format(uuid)
// @Param review body ReviewJudgmentOutcomeRequest true "The right outcome"
// @Success 200 {object} api.APIResponse{data=ReviewJudgmentOutcomeResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /judgment-outcomes/{id}/review [post]
func (h *JudgmentHandler) ReviewJudgmentOutcome(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "judgment outcome")
	if !ok {
		return
	}
	run, ok := runQuery(c)
	if !ok {
		return
	}
	var req ReviewJudgmentOutcomeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid review", "right_outcome is required")
		return
	}
	view, err := h.store.ReviewOutcome(c.Request.Context(), id, judgment.ReviewRequest{
		RightOutcome: req.RightOutcome, KeepCurrentDate: req.KeepCurrentDate,
	})
	if err != nil {
		switch {
		case errors.Is(err, judgment.ErrInvalidRightOutcome):
			api.SendValidationError(c, "Invalid right_outcome", "right_outcome must be one of the question's outcome values")
		case errors.Is(err, judgment.ErrInvalidKeepCurrentDate):
			api.SendValidationError(c, "Invalid keep_current_date", "keep_current_date is allowed only with an open commitment")
		case errors.Is(err, judgment.ErrNotFound):
			api.SendNotFound(c, "Judgment outcome")
		default:
			api.RespondInternal(c, err)
		}
		return
	}
	proposals, err := h.store.ThreadProposalsOf(c.Request.Context(), view.JudgmentID, run)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	api.SendSuccess(c, http.StatusOK, ReviewJudgmentOutcomeResponse{
		JudgmentOutcomeResponse: outcomeResponse(view), Proposals: benchProposalResponses(proposals),
	}, nil)
}

// ConfirmNones labels a proposal's negative decisions.
// @Summary Confirm a proposal's Nones
// @Description Labels every negative decision of the judgment's proposal that has no label from any proposal with its judged value, as ordinary reviews that change nothing live, and returns the proposals of its thread as the bench now shows them, narrowed to run_id's judgments when it is given. A decision some proposal on the same content holds as an item without a label, or as a reply its own commitment absorbs without a label, is not labeled and stays to review. It records all of its labels or none. A proposal with an item, or one holding no negative decision it would label, is refused.
// @Tags judgments
// @Produce json
// @Param id path string true "Judgment ID" format(uuid)
// @Param run_id query string false "Run the bench is narrowed to" format(uuid)
// @Success 200 {object} api.APIResponse{data=BenchProposalsResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Failure 409 {object} api.APIResponse
// @Router /judgments/{id}/confirm-nones [post]
func (h *JudgmentHandler) ConfirmNones(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "judgment")
	if !ok {
		return
	}
	run, ok := runQuery(c)
	if !ok {
		return
	}
	if err := h.store.ConfirmNones(c.Request.Context(), id); err != nil {
		switch {
		case errors.Is(err, judgment.ErrConfirmNonesRefused):
			api.SendConflict(c, "The proposal has an item or no negative decision Confirm Nones would label")
		case errors.Is(err, judgment.ErrNotFound):
			api.SendNotFound(c, "Judgment")
		default:
			api.RespondInternal(c, err)
		}
		return
	}
	proposals, err := h.store.ThreadProposalsOf(c.Request.Context(), id, run)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	api.SendSuccess(c, http.StatusOK, BenchProposalsResponse{Proposals: benchProposalResponses(proposals)}, nil)
}

// CloseBenchRow records the user closing a row on the bench.
// @Summary Close a bench row
// @Description Records the user closing a row: every negative decision without a label of each given proposal (the row's) that holds a label, from whichever proposal on the same content it was made in, is labeled with its judged value, as ordinary reviews that change nothing live. A decision some proposal on the same content holds as an item without a label, or as a reply its own commitment absorbs without a label, is not labeled and stays to review, and a proposal holding no label gains none. It records all of its labels or none, and returns the given proposals as the bench now shows them, narrowed to run_id's appearances when it is given, with what remains to review across them. An unknown judgment is not found, with nothing recorded.
// @Tags judgments
// @Accept json
// @Produce json
// @Param run_id query string false "Run the bench is narrowed to" format(uuid)
// @Param close body CloseBenchRowRequest true "The row's judgments"
// @Success 200 {object} api.APIResponse{data=CloseBenchRowResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /judgments/close [post]
func (h *JudgmentHandler) CloseBenchRow(c *gin.Context) {
	run, ok := runQuery(c)
	if !ok {
		return
	}
	var req CloseBenchRowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid close", "judgment_ids must name at least one judgment")
		return
	}
	ids := make([]uuid.UUID, len(req.JudgmentIDs))
	for i, raw := range req.JudgmentIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			api.SendValidationError(c, "Invalid judgment_ids", "each judgment ID must be a UUID")
			return
		}
		ids[i] = id
	}
	if err := h.store.CloseRow(c.Request.Context(), ids); err != nil {
		if errors.Is(err, judgment.ErrNotFound) {
			api.SendNotFound(c, "Judgment")
			return
		}
		api.RespondInternal(c, err)
		return
	}
	proposals, err := h.store.Proposals(c.Request.Context(), ids, run)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	toReview, status := judgment.RowStatus(proposals)
	out := CloseBenchRowResponse{Proposals: benchProposalResponses(proposals), ToReview: toReview}
	if status != nil {
		s := string(*status)
		out.ReviewStatus = &s
	}
	api.SendSuccess(c, http.StatusOK, out, nil)
}

// runQuery parses the optional run_id a save carries for the run the bench is
// narrowed to; it answers a validation error and false for a malformed one.
func runQuery(c *gin.Context) (*judgment.RunID, bool) {
	raw, ok := c.GetQuery("run_id")
	if !ok {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		api.SendValidationError(c, "Invalid run_id", "run_id must be a UUID")
		return nil, false
	}
	return &id, true
}

func benchProposalResponses(proposals []judgment.Proposal) []BenchProposalResponse {
	out := make([]BenchProposalResponse, len(proposals))
	for i, p := range proposals {
		out[i] = benchProposalResponse(p)
	}
	return out
}

func outcomeResponse(o judgment.OutcomeView) JudgmentOutcomeResponse {
	out := JudgmentOutcomeResponse{
		ID: o.ID.String(), Question: string(o.Question), Outcome: o.Outcome,
		Confidence: o.Confidence, State: string(o.State),
	}
	if o.ContactID != nil {
		contact := o.ContactID.String()
		out.ContactID = &contact
	}
	if o.DueDate != nil {
		due := o.DueDate.Format("2006-01-02")
		out.DueDate = &due
	}
	if o.Review != nil {
		out.Review = &JudgmentOutcomeReviewResponse{
			Verdict: string(o.Review.Verdict), RightOutcome: o.Review.RightOutcome, ReviewedAt: o.Review.ReviewedAt,
		}
	}
	return out
}
