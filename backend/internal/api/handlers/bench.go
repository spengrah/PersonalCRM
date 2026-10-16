package handlers

import (
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// BenchHandler serves the all-interactions view, where the user reviews
// judgments.
type BenchHandler struct {
	bench *judgment.Bench
}

// NewBenchHandler creates the all-interactions view's handler.
func NewBenchHandler(bench *judgment.Bench) *BenchHandler {
	return &BenchHandler{bench: bench}
}

// RegisterBenchRoutes wires the all-interactions view onto a group whose
// middleware already enforces the global API key:
//
//   - GET /api/v1/interactions
//   - GET /api/v1/interactions/threads/:thread_id/content
func RegisterBenchRoutes(v1 *gin.RouterGroup, handler *BenchHandler) {
	v1.GET("/interactions", handler.ListInteractions)
	v1.GET("/interactions/threads/:thread_id/content", handler.GetThreadContent)
}

// ListInteractions returns one page of every interaction across contacts.
// @Summary List all interactions
// @Description Every interaction across contacts, newest first: one row per interaction, naming each participant contact. Filters combine. run_id keeps the rows of threads with a judgment appearing in the run, and each row's proposals to those judgments. Each row carries the proposals of the thread judgments whose latest content it holds, compiled into the answers the user reviews.
// @Tags interactions
// @Produce json
// @Param page query int false "Page number" default(1)
// @Param limit query int false "Items per page" default(20)
// @Param contact_id query string false "Participant contact ID" format(uuid)
// @Param type query string false "Interaction type" Enums(message, email, calendar, call, other)
// @Param run_id query string false "Judgment run ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=AllInteractionsResponse}
// @Failure 400 {object} api.APIResponse
// @Router /interactions [get]
func (h *BenchHandler) ListInteractions(c *gin.Context) {
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
	query := judgment.BenchQuery{Limit: int32(limit64), Offset: int32(offset64)}
	for _, p := range []struct {
		name string
		dst  **uuid.UUID
	}{{"contact_id", &query.ContactID}, {"run_id", &query.Run}} {
		raw, ok := c.GetQuery(p.name)
		if !ok {
			continue
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			api.SendValidationError(c, "Invalid "+p.name, p.name+" must be a UUID")
			return
		}
		*p.dst = &id
	}
	if raw, ok := c.GetQuery("type"); ok {
		keys := make([]string, len(service.InteractionTypes))
		for i, t := range service.InteractionTypes {
			keys[i] = string(t.Key)
		}
		if !slices.Contains(keys, raw) {
			api.SendValidationError(c, "Invalid type", "type must be one of "+strings.Join(keys, ", "))
			return
		}
		typ := service.InteractionType(raw)
		query.Type = &typ
	}

	page, err := h.bench.List(c.Request.Context(), query)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	items := make([]AllInteractionItemResponse, len(page.Rows))
	for i, row := range page.Rows {
		items[i] = allInteractionItemResponse(row)
	}
	options := make([]InteractionTypeOptionResponse, len(service.InteractionTypes))
	for i, t := range service.InteractionTypes {
		options[i] = InteractionTypeOptionResponse{Key: string(t.Key), Label: t.Label}
	}
	api.SendSuccess(c, http.StatusOK, AllInteractionsResponse{Items: items, TypeOptions: options},
		&api.Meta{Pagination: api.BuildPaginationMeta(int(page64), int(limit64), int64(page.Total))})
}

// GetThreadContent returns a judged thread's whole conversation.
// @Summary Get a judged thread's content
// @Description The thread's messages, oldest first, each once however many per-contact copies hold it: the content its judgments read up to their cut-offs, and the content after them. A meeting's notes are its row's content and are not listed.
// @Tags interactions
// @Produce json
// @Param thread_id path string true "Thread (venue) ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=BenchThreadContentResponse}
// @Failure 400 {object} api.APIResponse
// @Router /interactions/threads/{thread_id}/content [get]
func (h *BenchHandler) GetThreadContent(c *gin.Context) {
	thread, ok := api.ParseUUIDParam(c, "thread_id", "thread")
	if !ok {
		return
	}
	content, err := h.bench.ThreadContent(c.Request.Context(), thread)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}
	messages := make([]ContentMessageResponse, len(content.Messages))
	for i, m := range content.Messages {
		messages[i] = ContentMessageResponse{
			ID: m.Key, Sender: messageSender(m, content.Names), IsOutgoing: m.FromUser, SentAt: m.SentAt,
			Body: m.Text, Source: strings.SplitN(m.Key, ":", 2)[0], ContentRef: m.Key,
		}
	}
	api.SendSuccess(c, http.StatusOK, BenchThreadContentResponse{Messages: messages}, nil)
}

// messageSender is the name the bench gives a thread message's sender: "Me"
// for the user's own, else the contact's name, else "Unknown".
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

func allInteractionItemResponse(row judgment.BenchRow) AllInteractionItemResponse {
	participants := make([]InteractionParticipantResponse, len(row.Participants))
	for i, p := range row.Participants {
		participants[i] = InteractionParticipantResponse{ContactID: p.ContactID.String(), Name: p.Name}
	}
	out := AllInteractionItemResponse{
		Item:             interactionListItemResponse(row.TimelineItem),
		ContextContactID: row.Participants[0].ContactID.String(),
		Participants:     participants,
		Proposals:        make([]BenchProposalResponse, len(row.Proposals)),
		ToReview:         row.ToReview,
	}
	if row.Preview != nil {
		out.Preview = &InteractionPreviewResponse{Text: row.Preview.Text, Note: row.Preview.Note}
		if !row.Preview.Note {
			out.Preview.Sender = &row.Preview.Sender
		}
	}
	for i, p := range row.Proposals {
		out.Proposals[i] = benchProposalResponse(p)
	}
	return out
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
		Production: p.Production, ContextKind: string(p.Kind), CutOff: p.CutOff, ReusedAt: p.ReusedAt,
		ReplyLists: p.ReplyLists, Items: make([]BenchItemResponse, len(p.Items)),
		Additions: make([]BenchAdditionResponse, len(p.Additions)), ToReview: p.ToReview,
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
