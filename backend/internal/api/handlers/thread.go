package handlers

import (
	"errors"
	"math"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/judgment"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ThreadHandler serves the Threads tab: the threads list, a thread with the
// nights of a run, the review runs, and the labels the user records on a thread.
type ThreadHandler struct {
	threads *judgment.Threads
}

// NewThreadHandler creates the Threads tab's handler.
func NewThreadHandler(threads *judgment.Threads) *ThreadHandler {
	return &ThreadHandler{threads: threads}
}

// ListThreads returns one page of the threads.
// @Summary List threads
// @Description One row per thread a method can judge (each DM, group chat, email thread and meeting holding content), newest latest message first. venue and contact_id narrow the list and combine. With run_id, only the threads the run judged are listed, in the same order, each adding its judged count and its status, and the response adds the run's summary.
// @Tags threads
// @Produce json
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Param venue query string false "Venue" Enums(whatsapp, telegram, imessage, email, calendar)
// @Param contact_id query string false "Participant contact ID" format(uuid)
// @Param run_id query string false "Judgment run ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=ThreadListResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /threads [get]
func (h *ThreadHandler) ListThreads(c *gin.Context) {
	page64, limit64, valid := parseInteractionPagination(c)
	if !valid {
		return
	}
	limit64 = min(limit64, 100)
	offset64 := (page64 - 1) * limit64
	if offset64 > math.MaxInt32 {
		api.SendValidationError(c, "Invalid pagination", "page offset is too large")
		return
	}
	query, ok := threadQuery(c)
	if !ok {
		return
	}
	query.Limit, query.Offset = int32(limit64), int32(offset64)
	page, err := h.threads.List(c.Request.Context(), query)
	if err != nil {
		h.fail(c, err)
		return
	}
	items := make([]ThreadRowResponse, len(page.Rows))
	for i, row := range page.Rows {
		items[i] = threadRowResponse(row)
	}
	out := ThreadListResponse{Items: items}
	if s := page.Summary; s != nil {
		out.Summary = &ThreadRunSummaryResponse{Judged: s.Judged, ToReview: s.ToReview, NothingFlagged: s.NothingFlagged, Reviewed: s.Reviewed}
	}
	api.SendSuccess(c, http.StatusOK, out, &api.Meta{Pagination: api.BuildPaginationMeta(int(page64), int(limit64), int64(page.Total))})
}

// GetThread returns a thread.
// @Summary Get a thread
// @Description The thread's header, every message once in thread order, and its place in the list that venue, contact_id and run_id keep. With run_id, it adds the run's nights, one per cut-off at which the run recorded or reused a judgment of the thread, each placing its messages once with the flags they show, its clearing decisions, its reply proposal and its status.
// @Tags threads
// @Produce json
// @Param thread_id path string true "Thread (venue) ID" format(uuid)
// @Param venue query string false "Venue" Enums(whatsapp, telegram, imessage, email, calendar)
// @Param contact_id query string false "Participant contact ID" format(uuid)
// @Param run_id query string false "Judgment run ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=ThreadReadResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /threads/{thread_id} [get]
func (h *ThreadHandler) GetThread(c *gin.Context) {
	thread, ok := api.ParseUUIDParam(c, "thread_id", "thread")
	if !ok {
		return
	}
	query, ok := threadQuery(c)
	if !ok {
		return
	}
	read, err := h.threads.Read(c.Request.Context(), thread, query)
	if err != nil {
		h.fail(c, err)
		return
	}
	out := ThreadReadResponse{Thread: threadRowResponse(read.Row), Messages: make([]ThreadMessageResponse, len(read.Messages))}
	for i, m := range read.Messages {
		out.Messages[i] = ThreadMessageResponse{MessageKey: m.Key, Sender: messageSender(m, read.Names), SentAt: m.SentAt, Text: m.Text}
	}
	if p := read.Position; p != nil {
		out.Position = &ThreadPositionResponse{Index: p.Index, Total: p.Total}
		if p.Previous != nil {
			previous := p.Previous.String()
			out.Position.PreviousThreadID = &previous
		}
		if p.Next != nil {
			next := p.Next.String()
			out.Position.NextThreadID = &next
		}
	}
	if read.Review != nil {
		review := threadReviewResponse(read.Review)
		out.Review = &review
	}
	api.SendSuccess(c, http.StatusOK, out, nil)
}

// ListReviewRuns returns the runs the user reviews.
// @Summary List review runs
// @Description The historical and scheduled runs of the compared methods, newest first, each with its kind, method, window and start. A scorecard's rerun and a run of a method that is not compared are never listed.
// @Tags threads
// @Produce json
// @Success 200 {object} api.APIResponse{data=ReviewRunsResponse}
// @Router /review-runs [get]
func (h *ThreadHandler) ListReviewRuns(c *gin.Context) {
	runs, err := h.threads.ReviewRuns(c.Request.Context())
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	out := make([]ReviewRunResponse, len(runs))
	for i, r := range runs {
		out[i] = reviewRunResponse(r)
	}
	api.SendSuccess(c, http.StatusOK, ReviewRunsResponse{Runs: out}, nil)
}

// LabelFlag records whether a message holds a flag kind.
// @Summary Label a flag decision
// @Description Records whether the message holds the flag kind as the label of the flag decision (thread, message, kind), replacing any earlier label. The label is shared by every method, run and cut-off that judged the message and kind, and changes nothing live. Answers with the thread's review state in the run. A kind outside the flag kinds, a message the thread does not hold, a missing run_id and an unknown thread or run are refused, recording nothing.
// @Tags threads
// @Accept json
// @Produce json
// @Param thread_id path string true "Thread (venue) ID" format(uuid)
// @Param run_id query string true "Judgment run ID" format(uuid)
// @Param label body FlagLabelRequest true "The label"
// @Success 200 {object} api.APIResponse{data=ThreadReviewResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /threads/{thread_id}/flag-labels [post]
func (h *ThreadHandler) LabelFlag(c *gin.Context) {
	thread, run, ok := threadAndRun(c)
	if !ok {
		return
	}
	var req FlagLabelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid flag label", "message_key, kind and holds are required")
		return
	}
	review, err := h.threads.LabelFlag(c.Request.Context(), run, thread, req.MessageKey, judgment.FlagKind(req.Kind), *req.Holds)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.sendReview(c, review)
}

// LabelClearing records whether a commitment is done as of a cut-off.
// @Summary Label a clearing decision
// @Description Records whether the commitment is done as of the cut-off as the label of the clearing decision (thread, commitment message, cut-off), replacing any earlier label. The label is shared by every method's clearing decision of that commitment at that cut-off, and changes nothing live. Answers with the thread's review state in the run. A clearing decision no judgment of the thread made, a missing run_id and an unknown thread or run are refused, recording nothing.
// @Tags threads
// @Accept json
// @Produce json
// @Param thread_id path string true "Thread (venue) ID" format(uuid)
// @Param run_id query string true "Judgment run ID" format(uuid)
// @Param label body ClearingLabelRequest true "The label"
// @Success 200 {object} api.APIResponse{data=ThreadReviewResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /threads/{thread_id}/clearing-labels [post]
func (h *ThreadHandler) LabelClearing(c *gin.Context) {
	thread, run, ok := threadAndRun(c)
	if !ok {
		return
	}
	var req ClearingLabelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid clearing label", "commitment_message_key, cut_off and done are required")
		return
	}
	review, err := h.threads.LabelClearing(c.Request.Context(), run, thread, req.CommitmentMessageKey, req.CutOff, *req.Done)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.sendReview(c, review)
}

// CloseThread records the user finishing a thread's review.
// @Summary Close a thread
// @Description For each night of the run that holds a label, labels as not holding every flag decision without a label of its judged messages, for all three flag kinds, and labels the night's negative reply decisions without a label with their judged values. A flag decision that a compared method's judgment shows as a flag without a label stays unlabeled, whichever run shows it. A night holding no label gains nothing, an unjudged message gains no label and a label that exists is never replaced. It records all of its labels or none, and answers with the thread's review state in the run.
// @Tags threads
// @Produce json
// @Param thread_id path string true "Thread (venue) ID" format(uuid)
// @Param run_id query string true "Judgment run ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=ThreadReviewResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Router /threads/{thread_id}/close [post]
func (h *ThreadHandler) CloseThread(c *gin.Context) {
	thread, run, ok := threadAndRun(c)
	if !ok {
		return
	}
	review, err := h.threads.Close(c.Request.Context(), run, thread)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.sendReview(c, review)
}

// ConfirmNones labels a night that shows nothing.
// @Summary Confirm a night's Nones
// @Description Labels, for the night the judgment makes in the run, as not holding every flag decision without a label of its judged messages that no judgment of a compared method shows as a flag without a label, and labels the night's negative reply decisions without a label with their judged values, as ordinary labels that change nothing live. It records all of its labels or none, and answers with the thread's review state in the run. A night that shows a flag chip, a clearing decision or a reply item, or holds a label, is refused as a conflict, recording nothing. A judgment that is no night of the run is not found.
// @Tags judgments
// @Produce json
// @Param id path string true "Judgment ID" format(uuid)
// @Param run_id query string true "Judgment run ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=ThreadReviewResponse}
// @Failure 400 {object} api.APIResponse
// @Failure 404 {object} api.APIResponse
// @Failure 409 {object} api.APIResponse
// @Router /judgments/{id}/confirm-nones [post]
func (h *ThreadHandler) ConfirmNones(c *gin.Context) {
	id, ok := api.ParseUUIDParam(c, "id", "judgment")
	if !ok {
		return
	}
	run, ok := runQuery(c)
	if !ok {
		return
	}
	if run == nil {
		api.SendValidationError(c, "Missing run_id", "run_id is required")
		return
	}
	review, err := h.threads.ConfirmNight(c.Request.Context(), *run, id)
	if err != nil {
		switch {
		case errors.Is(err, judgment.ErrConfirmNonesRefused):
			api.SendConflict(c, "The night shows a chip, a clearing decision or a reply item, or holds a label")
		case errors.Is(err, judgment.ErrNotFound):
			api.SendNotFound(c, "Judgment")
		case errors.Is(err, judgment.ErrRunNotFound):
			api.SendNotFound(c, "Run")
		default:
			api.RespondInternal(c, err)
		}
		return
	}
	api.SendSuccess(c, http.StatusOK, threadReviewResponse(review), nil)
}

func (h *ThreadHandler) sendReview(c *gin.Context, review *judgment.ThreadReview) {
	api.SendSuccess(c, http.StatusOK, threadReviewResponse(review), nil)
}

// fail answers an error of the Threads service.
func (h *ThreadHandler) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, judgment.ErrThreadNotFound):
		api.SendNotFound(c, "Thread")
	case errors.Is(err, judgment.ErrRunNotFound):
		api.SendNotFound(c, "Run")
	case errors.Is(err, judgment.ErrInvalidFlagKind):
		api.SendValidationError(c, "Invalid kind", "kind must be one of commitment, big_moment, claim")
	case errors.Is(err, judgment.ErrInvalidMessage):
		api.SendValidationError(c, "Invalid message_key", "the thread holds no such message")
	case errors.Is(err, judgment.ErrInvalidClearing):
		api.SendValidationError(c, "Invalid clearing decision", "no judgment of the thread made that clearing decision")
	default:
		api.RespondInternal(c, err)
	}
}

// threadQuery parses the optional venue, contact_id and run_id filters a
// threads read carries.
func threadQuery(c *gin.Context) (judgment.ThreadQuery, bool) {
	var q judgment.ThreadQuery
	if raw, ok := c.GetQuery("venue"); ok {
		venue, valid := judgment.ParseThreadVenue(raw)
		if !valid {
			api.SendValidationError(c, "Invalid venue", "venue must be one of whatsapp, telegram, imessage, email, calendar")
			return q, false
		}
		q.Venue = &venue
	}
	for _, p := range []struct {
		name string
		dst  **uuid.UUID
	}{{"contact_id", &q.ContactID}, {"run_id", &q.Run}} {
		raw, ok := c.GetQuery(p.name)
		if !ok {
			continue
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			api.SendValidationError(c, "Invalid "+p.name, p.name+" must be a UUID")
			return q, false
		}
		*p.dst = &id
	}
	return q, true
}

// threadAndRun parses the thread of the path and the run a save names; the
// run is required.
func threadAndRun(c *gin.Context) (uuid.UUID, uuid.UUID, bool) {
	raw, ok := c.GetQuery("run_id")
	if !ok {
		api.SendValidationError(c, "Missing run_id", "run_id is required")
		return uuid.Nil, uuid.Nil, false
	}
	run, err := uuid.Parse(raw)
	if err != nil {
		api.SendValidationError(c, "Invalid run_id", "run_id must be a UUID")
		return uuid.Nil, uuid.Nil, false
	}
	thread, ok := api.ParseUUIDParam(c, "thread_id", "thread")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return thread, run, true
}

func threadRowResponse(row judgment.ThreadRow) ThreadRowResponse {
	participants := make([]ThreadParticipantResponse, len(row.Participants))
	for i, p := range row.Participants {
		participants[i] = ThreadParticipantResponse{ContactID: p.ContactID.String(), Name: p.Name}
	}
	out := ThreadRowResponse{
		ThreadID: row.ID.String(), Venue: string(row.Venue), IsGroup: row.IsGroup, Title: row.Title,
		Participants: participants, LatestAt: row.LatestAt, MessageCount: row.MessageCount,
	}
	if r := row.Review; r != nil {
		out.Review = &ThreadRowReviewResponse{JudgedCount: r.JudgedCount, Status: string(r.Status), ToReview: r.ToReview}
	}
	return out
}

func reviewRunResponse(r judgment.ReviewRun) ReviewRunResponse {
	return ReviewRunResponse{
		ID: r.ID.String(), Kind: string(r.Kind), MethodKey: r.Method.Key, MethodVersion: r.Method.Version,
		From: r.From, To: r.To, StartedAt: r.StartedAt,
	}
}

func threadReviewResponse(r *judgment.ThreadReview) ThreadReviewResponse {
	out := ThreadReviewResponse{
		Run: reviewRunResponse(r.Run), Status: string(r.Status), ToReview: r.ToReview, JudgedCount: r.JudgedCount,
		Nights: make([]NightResponse, len(r.Nights)), ContextMessages: r.ContextMessages, LaterMessages: r.LaterMessages,
	}
	for i, n := range r.Nights {
		out.Nights[i] = nightResponse(n, r.Names)
	}
	return out
}

func nightResponse(n judgment.Night, names map[uuid.UUID]string) NightResponse {
	out := NightResponse{
		JudgmentID: n.JudgmentID.String(), MethodKey: n.Method.Key, MethodVersion: n.Method.Version, CutOff: n.CutOff,
		WindowStart: n.WindowStart, NewMessageCount: n.NewMessageCount,
		Messages:  make([]NightMessageResponse, len(n.Messages)),
		Clearings: make([]NightClearingResponse, len(n.Clearings)), ClosestUnflagged: make([]ClosestUnflaggedResponse, len(n.Closest)),
		Proposal: benchProposalResponse(*n.Proposal), ToReview: n.ToReview, ConfirmNones: n.ConfirmNones,
		HoldsLabel: n.HoldsLabel, LabeledNone: n.LabeledNone,
	}
	for i, m := range n.Messages {
		out.Messages[i] = NightMessageResponse{MessageKey: m.Key, Judged: m.Judged, Chips: make([]FlagChipResponse, len(m.Chips))}
		if m.Reason != "" {
			reason := string(m.Reason)
			out.Messages[i].Reason = &reason
		}
		for j, c := range m.Chips {
			out.Messages[i].Chips[j] = FlagChipResponse{Kind: string(c.Kind), State: string(c.State), Probability: c.Probability, ClearedAt: c.ClearedAt}
		}
	}
	for i, c := range n.Clearings {
		out.Clearings[i] = NightClearingResponse{
			CommitmentMessageKey: c.CommitmentMessageKey, CommitmentSentAt: c.CommitmentSentAt, CutOff: c.CutOff,
			Done: c.Done, Probability: c.Probability, Label: string(c.Label),
		}
	}
	for i, c := range n.Closest {
		out.ClosestUnflagged[i] = ClosestUnflaggedResponse{Kind: string(c.Kind)}
		if m := c.Message; m != nil {
			p := c.Probability
			out.ClosestUnflagged[i].Message = &ClosestMessageResponse{MessageKey: m.Key, Sender: messageSender(*m, names), SentAt: m.SentAt}
			out.ClosestUnflagged[i].Probability = &p
		}
	}
	return out
}

// messageSender is the name the Threads tab gives a thread message's sender:
// "Me" for the user's own, else the contact's name, else "Unknown".
func messageSender(m judgment.ThreadMessage, names map[uuid.UUID]string) string {
	switch {
	case m.FromUser:
		return "Me"
	case m.Sender != nil && names[*m.Sender] != "":
		return names[*m.Sender]
	default:
		return "Unknown"
	}
}

func benchProposalResponse(p judgment.Proposal) BenchProposalResponse {
	party := func(id *uuid.UUID) *BenchPartyResponse {
		if id == nil {
			return nil
		}
		return &BenchPartyResponse{ContactID: id.String(), Name: p.Names[*id]}
	}
	optional := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	out := BenchProposalResponse{
		JudgmentID: p.JudgmentID.String(), ThreadID: p.Thread.String(), MethodKey: p.Method.Key, MethodVersion: p.Method.Version,
		Production: p.Production, ContextKind: string(p.Kind), CutOff: p.CutOff,
		ReplyLists: p.ReplyLists, Items: make([]BenchItemResponse, len(p.Items)),
		Additions: make([]BenchAdditionResponse, len(p.Additions)), Lists: make([]BenchListResponse, len(p.Lists)),
	}
	for i, l := range p.Lists {
		out.Lists[i] = BenchListResponse{List: string(l.List), NonesReviewed: l.NonesReviewed}
		if c := l.Closest; c != nil {
			out.Lists[i].Closest = &BenchClosestResponse{Contact: party(c.Contact), Debtor: optional(string(c.Debtor)), Percent: c.Percent}
		}
	}
	for i, item := range p.Items {
		acts := make([]BenchActResponse, len(item.Acts))
		for j, a := range item.Acts {
			acts[j] = BenchActResponse{Act: string(a.Act), RightOutcome: a.Review.RightOutcome, KeepCurrentDate: a.Review.KeepCurrentDate}
		}
		r := BenchItemResponse{
			OutcomeID: item.OutcomeID.String(), List: string(item.List), Contact: party(item.Contact),
			Debtor: optional(string(item.Debtor)), State: optional(string(item.State)), Confidence: item.Confidence,
			DueDate: dateString(item.DueDate), Mark: optional(string(item.Mark)), Acts: acts,
			LiveDueDate: dateString(item.LiveDueDate),
		}
		if item.Change != nil {
			r.Change = &BenchItemChangeResponse{Kind: string(item.Change.Kind), DueDate: dateString(item.Change.DueDate), Confidence: item.Change.Confidence}
		}
		if s := item.Source; s != nil {
			r.Source = &BenchSourceMessageResponse{ContentRef: s.Key, Sender: messageSender(*s, p.Names), SentAt: s.SentAt, Text: s.Text}
		}
		out.Items[i] = r
	}
	for i, a := range p.Additions {
		out.Additions[i] = BenchAdditionResponse{
			List: string(a.List), Contact: party(a.Contact), Debtor: optional(string(a.Debtor)),
			OutcomeID: a.OutcomeID.String(), RightOutcome: a.Review.RightOutcome,
		}
	}
	return out
}

// dateString is a calendar date as the wire carries it (YYYY-MM-DD); nil
// stays nil.
func dateString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format("2006-01-02")
	return &s
}
