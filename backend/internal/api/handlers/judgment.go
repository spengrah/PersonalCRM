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
// @Description Records the right outcome the user states for an outcome of any state and returns the outcome with the verdict the server derives: confirmed when it equals the judged outcome, rejected when it differs and has no effect, corrected otherwise. Reviewing an applied or claim outcome gives the thread the right outcome's effect. keep_current_date, with an open commitment's right outcome, keeps the obligation's current date against the judged later one, and the verdict is corrected.
// @Tags judgments
// @Accept json
// @Produce json
// @Param id path string true "Outcome ID" format(uuid)
// @Param review body ReviewJudgmentOutcomeRequest true "The right outcome"
// @Success 200 {object} api.APIResponse{data=JudgmentOutcomeResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /judgment-outcomes/{id}/review [post]
func (h *JudgmentHandler) ReviewJudgmentOutcome(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "judgment outcome")
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
	api.SendSuccess(c, http.StatusOK, outcomeResponse(view), nil)
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
