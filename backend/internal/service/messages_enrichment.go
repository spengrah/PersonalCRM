package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MessagesEnrichmentRepository is the messages_message surface the enrichment
// exchange uses. Its writes fill a missing body, add a message as evidence,
// remove a member attribution, or store a reaction's facts, and nothing else.
type MessagesEnrichmentRepository interface {
	ListBodylessGuids(ctx context.Context, after *string, limit int32) ([]string, error)
	FillBody(ctx context.Context, guid, body string) (bool, error)
	ListGuidsWithoutReactionFacts(ctx context.Context, after *string, limit int32) ([]string, error)
	SetReaction(ctx context.Context, guid string, reaction repository.MessageReaction) (bool, error)
	GetMessage(ctx context.Context, guid string) (*repository.MessagesMessage, error)
	ListGroupBurstWindows(ctx context.Context, f repository.GroupBurstWindowFilter) ([]repository.GroupBurstWindow, error)
	GuidExists(ctx context.Context, guid string) (bool, error)
	AddGroupEvidence(ctx context.Context, p repository.AddGroupEvidenceParams) (bool, error)
	ClearOutboundPeer(ctx context.Context, guid string) (bool, error)
}

// MessagesEnrichmentService serves the Mac daemon's re-scans of stored
// messages. It calls no ingest, aggregation, cadence, follow-up or event code:
// filling a body, adding a message as evidence, clearing a member attribution
// and storing a reaction's facts are its only writes.
type MessagesEnrichmentService struct {
	messages MessagesEnrichmentRepository
}

func NewMessagesEnrichmentService(messages MessagesEnrichmentRepository) *MessagesEnrichmentService {
	return &MessagesEnrichmentService{messages: messages}
}

// GuidItem is one listed row; the guid is all the daemon needs to look the
// message up locally.
type GuidItem struct {
	Guid string `json:"guid"`
}

// GuidPage is one page of listed guids. NextAfter is the last listed guid when
// more remain, else nil.
type GuidPage struct {
	Items     []GuidItem `json:"items"`
	NextAfter *string    `json:"next_after"`
}

// ListMissingBodies lists up to limit guids of stored, non-deleted rows whose
// text is NULL or empty, in guid order, strictly after the given guid.
func (s *MessagesEnrichmentService) ListMissingBodies(ctx context.Context, after *string, limit int) (*GuidPage, error) {
	page, err := listGuidPage(ctx, s.messages.ListBodylessGuids, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list body-less messages: %w", err)
	}
	return page, nil
}

// ListWithoutReactionFacts lists up to limit guids of stored, non-deleted rows
// that carry no reaction kind, in guid order, strictly after the given guid.
func (s *MessagesEnrichmentService) ListWithoutReactionFacts(ctx context.Context, after *string, limit int) (*GuidPage, error) {
	page, err := listGuidPage(ctx, s.messages.ListGuidsWithoutReactionFacts, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list messages without reaction facts: %w", err)
	}
	return page, nil
}

func listGuidPage(ctx context.Context, list func(ctx context.Context, after *string, limit int32) ([]string, error), after *string, limit int) (*GuidPage, error) {
	// One row beyond the page tells whether another page remains.
	guids, err := list(ctx, after, int32(limit+1))
	if err != nil {
		return nil, err
	}
	page := &GuidPage{Items: []GuidItem{}}
	if len(guids) > limit {
		guids = guids[:limit]
		last := guids[limit-1]
		page.NextAfter = &last
	}
	for _, g := range guids {
		page.Items = append(page.Items, GuidItem{Guid: g})
	}
	return page, nil
}

// GroupChatWindow is the closed span of one burst of a group chat.
type GroupChatWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// GroupChatItem is one group chat with every distinct window of its live
// messages interactions.
type GroupChatItem struct {
	ChatGuid string            `json:"chat_guid"`
	Windows  []GroupChatWindow `json:"windows"`
}

// GroupChatsPage is one page of group chats. NextAfter is the last listed
// chat_guid when more remain, else nil.
type GroupChatsPage struct {
	Items     []GroupChatItem `json:"items"`
	NextAfter *string         `json:"next_after"`
}

// ListGroupChats lists up to limit group chats that have at least one live
// messages interaction linked to live rows, in chat_guid order and strictly
// after the given chat_guid. Each chat carries its distinct windows ordered by
// start then end.
func (s *MessagesEnrichmentService) ListGroupChats(ctx context.Context, after *string, limit int) (*GroupChatsPage, error) {
	// One chat beyond the page tells whether another page remains.
	chatLimit := int32(limit + 1)
	windows, err := s.messages.ListGroupBurstWindows(ctx, repository.GroupBurstWindowFilter{After: after, ChatLimit: &chatLimit})
	if err != nil {
		return nil, fmt.Errorf("list group burst windows: %w", err)
	}
	page := &GroupChatsPage{Items: []GroupChatItem{}}
	for _, w := range windows {
		window := GroupChatWindow{Start: w.Start.UTC(), End: w.End.UTC()}
		last := len(page.Items) - 1
		switch {
		case last < 0 || page.Items[last].ChatGuid != w.ChatGuid:
			page.Items = append(page.Items, GroupChatItem{ChatGuid: w.ChatGuid, Windows: []GroupChatWindow{window}})
		case page.Items[last].Windows[len(page.Items[last].Windows)-1] != window:
			page.Items[last].Windows = append(page.Items[last].Windows, window)
		}
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		lastChat := page.Items[limit-1].ChatGuid
		page.NextAfter = &lastChat
	}
	return page, nil
}

// EnrichmentOp names one kind of enrichment item.
type EnrichmentOp string

const (
	EnrichmentOpFillBody          EnrichmentOp = "fill_body"
	EnrichmentOpAddGroupMessage   EnrichmentOp = "add_group_message"
	EnrichmentOpClearOutboundPeer EnrichmentOp = "clear_outbound_peer"
	EnrichmentOpSetReaction       EnrichmentOp = "set_reaction"
)

// EnrichmentOutcome is what one enrichment item did.
type EnrichmentOutcome string

const (
	EnrichmentFilled        EnrichmentOutcome = "filled"
	EnrichmentUnchanged     EnrichmentOutcome = "unchanged"
	EnrichmentUnknown       EnrichmentOutcome = "unknown"
	EnrichmentAdded         EnrichmentOutcome = "added"
	EnrichmentOutsideWindow EnrichmentOutcome = "outside_window"
	EnrichmentCleared       EnrichmentOutcome = "cleared"
	EnrichmentMarked        EnrichmentOutcome = "marked"
)

// EnrichmentItem is one validated enrichment request item. Body is the body to
// fill (fill_body) or the body of the added message (add_group_message);
// ChatGuid, Sender, Outgoing and SentAt belong to add_group_message, whose
// Sender is nil for the user's own message; Reaction belongs to set_reaction.
type EnrichmentItem struct {
	Op       EnrichmentOp
	Guid     string
	Body     *string
	ChatGuid string
	Sender   *string
	Outgoing bool
	SentAt   time.Time
	Reaction *repository.MessageReaction
}

type EnrichmentResult struct {
	Guid    string            `json:"guid"`
	Outcome EnrichmentOutcome `json:"outcome"`
}

// EnrichmentTotals counts results per outcome; every key is always present.
type EnrichmentTotals struct {
	Filled        int `json:"filled"`
	Unchanged     int `json:"unchanged"`
	Unknown       int `json:"unknown"`
	Added         int `json:"added"`
	OutsideWindow int `json:"outside_window"`
	Cleared       int `json:"cleared"`
	Marked        int `json:"marked"`
}

type EnrichmentResponse struct {
	Results []EnrichmentResult `json:"results"`
	Totals  EnrichmentTotals   `json:"totals"`
}

// ApplyItems applies each item in request order and answers one result per
// item. hostID is the authenticated host that sent the request.
func (s *MessagesEnrichmentService) ApplyItems(ctx context.Context, hostID uuid.UUID, items []EnrichmentItem) (*EnrichmentResponse, error) {
	res := &EnrichmentResponse{Results: make([]EnrichmentResult, 0, len(items))}
	for _, item := range items {
		outcome, err := s.applyOne(ctx, hostID, item)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", item.Op, item.Guid, err)
		}
		res.Results = append(res.Results, EnrichmentResult{Guid: item.Guid, Outcome: outcome})
		switch outcome {
		case EnrichmentFilled:
			res.Totals.Filled++
		case EnrichmentUnchanged:
			res.Totals.Unchanged++
		case EnrichmentUnknown:
			res.Totals.Unknown++
		case EnrichmentAdded:
			res.Totals.Added++
		case EnrichmentOutsideWindow:
			res.Totals.OutsideWindow++
		case EnrichmentCleared:
			res.Totals.Cleared++
		case EnrichmentMarked:
			res.Totals.Marked++
		}
	}
	return res, nil
}

func (s *MessagesEnrichmentService) applyOne(ctx context.Context, hostID uuid.UUID, item EnrichmentItem) (EnrichmentOutcome, error) {
	switch item.Op {
	case EnrichmentOpFillBody:
		return s.fillOne(ctx, item)
	case EnrichmentOpAddGroupMessage:
		return s.addOne(ctx, hostID, item)
	case EnrichmentOpClearOutboundPeer:
		return s.clearOne(ctx, item)
	case EnrichmentOpSetReaction:
		return s.markOne(ctx, item)
	default:
		return "", fmt.Errorf("unknown op %q", item.Op)
	}
}

// fillOne fills the body of a stored row that has none. A row that already has
// a body keeps it (unchanged); a guid with no non-deleted row creates nothing
// (unknown).
func (s *MessagesEnrichmentService) fillOne(ctx context.Context, item EnrichmentItem) (EnrichmentOutcome, error) {
	filled, err := s.messages.FillBody(ctx, item.Guid, *item.Body)
	if err != nil {
		return "", err
	}
	if filled {
		return EnrichmentFilled, nil
	}
	// The row was not filled: it either already has a body or does not exist.
	return s.unchangedOrUnknown(ctx, item.Guid)
}

// addOne stores a missing group message as evidence when its time lies inside a
// window of its stored group chat. A guid any stored row holds, soft-deleted or
// not, is left alone (unchanged).
func (s *MessagesEnrichmentService) addOne(ctx context.Context, hostID uuid.UUID, item EnrichmentItem) (EnrichmentOutcome, error) {
	exists, err := s.messages.GuidExists(ctx, item.Guid)
	if err != nil {
		return "", err
	}
	if exists {
		return EnrichmentUnchanged, nil
	}
	containing, err := s.messages.ListGroupBurstWindows(ctx, repository.GroupBurstWindowFilter{ChatGuid: &item.ChatGuid, ContainsAt: &item.SentAt})
	if err != nil {
		return "", err
	}
	if len(containing) == 0 {
		return EnrichmentOutsideWindow, nil
	}
	peer := ""
	if item.Sender != nil {
		peer = *item.Sender
	}
	added, err := s.messages.AddGroupEvidence(ctx, repository.AddGroupEvidenceParams{
		Guid:       item.Guid,
		ChatGuid:   item.ChatGuid,
		PeerHandle: peer,
		Text:       item.Body,
		SentAt:     item.SentAt,
		IsOutgoing: item.Outgoing,
		MacHostID:  hostID,
	})
	if err != nil {
		return "", err
	}
	if !added {
		return EnrichmentUnchanged, nil
	}
	return EnrichmentAdded, nil
}

// clearOne removes the member attribution from a stored outbound group row. A
// row without attribution, an inbound row and a one-to-one row stay as they
// are (unchanged); a guid with no non-deleted row is unknown.
func (s *MessagesEnrichmentService) clearOne(ctx context.Context, item EnrichmentItem) (EnrichmentOutcome, error) {
	cleared, err := s.messages.ClearOutboundPeer(ctx, item.Guid)
	if err != nil {
		return "", err
	}
	if cleared {
		return EnrichmentCleared, nil
	}
	return s.unchangedOrUnknown(ctx, item.Guid)
}

// markOne stores a reaction's facts on a stored row that carries none. A row
// that already carries facts keeps them (unchanged); a guid with no non-deleted
// row creates nothing (unknown).
func (s *MessagesEnrichmentService) markOne(ctx context.Context, item EnrichmentItem) (EnrichmentOutcome, error) {
	marked, err := s.messages.SetReaction(ctx, item.Guid, *item.Reaction)
	if err != nil {
		return "", err
	}
	if marked {
		return EnrichmentMarked, nil
	}
	return s.unchangedOrUnknown(ctx, item.Guid)
}

func (s *MessagesEnrichmentService) unchangedOrUnknown(ctx context.Context, guid string) (EnrichmentOutcome, error) {
	if _, err := s.messages.GetMessage(ctx, guid); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return EnrichmentUnknown, nil
		}
		return "", err
	}
	return EnrichmentUnchanged, nil
}

// MutualPromoterTx promotes an interaction to mutual inside the caller's
// transaction. Satisfied by *ContactService.
type MutualPromoterTx interface {
	PromoteInteractionToMutualTx(ctx context.Context, tx pgx.Tx, interactionID, contactID uuid.UUID, replyAt time.Time) error
}

// GroupBurstPromoter applies R12: the user's group messages carry no member
// attribution, so each member inbound burst whose window contains one becomes
// mutual. Whether a message is such a reply is the stored credits_reply marker,
// read through MessagesMessageRepository.HasCreditingReplyBetween on both the
// ingest and the aggregation path. It promotes at the burst's own occurred_at, so the burst keeps its
// linked messages, window and time and its cadence effect is that of the
// existing promotion to mutual. Windows come from the one derivation,
// MessagesMessageRepository.ListGroupBurstWindows.
type GroupBurstPromoter struct {
	messages *repository.MessagesMessageRepository
	promoter MutualPromoterTx
}

func NewGroupBurstPromoter(messages *repository.MessagesMessageRepository, promoter MutualPromoterTx) *GroupBurstPromoter {
	return &GroupBurstPromoter{messages: messages, promoter: promoter}
}

// PromoteContainingTx promotes every live inbound burst of the stored row's
// chat whose window contains the row's sent time, when the row credits a reply.
// The row is the one the ingest upsert returned: for a guid already stored it
// is the stored row, not the incoming payload, so a message the enrichment
// added never promotes a burst by being ingested again.
func (p *GroupBurstPromoter) PromoteContainingTx(ctx context.Context, tx pgx.Tx, stored *repository.MessagesMessage) error {
	messages := p.messages.WithTx(tx)
	credits, err := messages.HasCreditingReplyBetween(ctx, stored.ChatGuid, stored.SentAt, stored.SentAt)
	if err != nil {
		return fmt.Errorf("read reply eligibility: %w", err)
	}
	if !credits {
		return nil
	}
	inbound := repository.InteractionDirectionInbound
	windows, err := messages.ListGroupBurstWindows(ctx, repository.GroupBurstWindowFilter{
		ChatGuid:   &stored.ChatGuid,
		Direction:  &inbound,
		ContainsAt: &stored.SentAt,
	})
	if err != nil {
		return fmt.Errorf("list bursts containing user message: %w", err)
	}
	return p.promoteAll(ctx, tx, windows)
}

// PromoteIfRepliedTx promotes the interaction when it is a live inbound burst
// whose window now contains a message that credits a reply. Callers run it
// after a burst gained messages.
func (p *GroupBurstPromoter) PromoteIfRepliedTx(ctx context.Context, tx pgx.Tx, interactionID uuid.UUID) error {
	inbound := repository.InteractionDirectionInbound
	messages := p.messages.WithTx(tx)
	windows, err := messages.ListGroupBurstWindows(ctx, repository.GroupBurstWindowFilter{
		InteractionID: &interactionID,
		Direction:     &inbound,
	})
	if err != nil {
		return fmt.Errorf("list burst window: %w", err)
	}
	var replied []repository.GroupBurstWindow
	for _, w := range windows {
		found, err := messages.HasCreditingReplyBetween(ctx, w.ChatGuid, w.Start, w.End)
		if err != nil {
			return fmt.Errorf("find reply in burst window: %w", err)
		}
		if found {
			replied = append(replied, w)
		}
	}
	return p.promoteAll(ctx, tx, replied)
}

func (p *GroupBurstPromoter) promoteAll(ctx context.Context, tx pgx.Tx, windows []repository.GroupBurstWindow) error {
	for _, w := range windows {
		if err := p.promoter.PromoteInteractionToMutualTx(ctx, tx, w.InteractionID, w.ContactID, w.OccurredAt); err != nil {
			return fmt.Errorf("promote burst %s to mutual: %w", w.InteractionID, err)
		}
	}
	return nil
}
