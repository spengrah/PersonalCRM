package service

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
)

// MessagesEnrichmentRepository is the messages_message surface the enrichment
// exchange uses. Its writes set a missing body and nothing else.
type MessagesEnrichmentRepository interface {
	ListBodylessGuids(ctx context.Context, after *string, limit int32) ([]string, error)
	FillBody(ctx context.Context, guid, body string) (bool, error)
	GetMessage(ctx context.Context, guid string) (*repository.MessagesMessage, error)
}

// MessagesEnrichmentService serves the Mac daemon's re-scan of stored
// body-less messages. It calls no ingest, aggregation, cadence, follow-up or
// event code: filling a body is the only write.
type MessagesEnrichmentService struct {
	messages MessagesEnrichmentRepository
}

func NewMessagesEnrichmentService(messages MessagesEnrichmentRepository) *MessagesEnrichmentService {
	return &MessagesEnrichmentService{messages: messages}
}

// MissingBodyItem is one listed body-less row; the guid is all the daemon
// needs to look the body up locally.
type MissingBodyItem struct {
	Guid string `json:"guid"`
}

// MissingBodiesPage is one page of body-less guids. NextAfter is the last
// listed guid when more remain, else nil.
type MissingBodiesPage struct {
	Items     []MissingBodyItem `json:"items"`
	NextAfter *string           `json:"next_after"`
}

// ListMissingBodies lists up to limit guids of stored, non-deleted rows whose
// text is NULL or empty, in guid order, strictly after the given guid.
func (s *MessagesEnrichmentService) ListMissingBodies(ctx context.Context, after *string, limit int) (*MissingBodiesPage, error) {
	// One row beyond the page tells whether another page remains.
	guids, err := s.messages.ListBodylessGuids(ctx, after, int32(limit+1))
	if err != nil {
		return nil, fmt.Errorf("list body-less messages: %w", err)
	}
	page := &MissingBodiesPage{Items: []MissingBodyItem{}}
	if len(guids) > limit {
		guids = guids[:limit]
		last := guids[limit-1]
		page.NextAfter = &last
	}
	for _, g := range guids {
		page.Items = append(page.Items, MissingBodyItem{Guid: g})
	}
	return page, nil
}

// FillBodyOutcome is what one fill_body item did.
type FillBodyOutcome string

const (
	FillBodyFilled    FillBodyOutcome = "filled"
	FillBodyUnchanged FillBodyOutcome = "unchanged"
	FillBodyUnknown   FillBodyOutcome = "unknown"
)

// FillBodyItem is one validated fill_body request item.
type FillBodyItem struct {
	Guid string
	Body string
}

type FillBodyResult struct {
	Guid    string          `json:"guid"`
	Outcome FillBodyOutcome `json:"outcome"`
}

// FillBodyTotals counts results per outcome; every key is always present.
type FillBodyTotals struct {
	Filled    int `json:"filled"`
	Unchanged int `json:"unchanged"`
	Unknown   int `json:"unknown"`
}

type FillBodiesResult struct {
	Results []FillBodyResult `json:"results"`
	Totals  FillBodyTotals   `json:"totals"`
}

// FillBodies fills the body of each named stored row that has none, in
// request order. A row that already has a body keeps it (unchanged); a guid
// with no non-deleted row creates nothing (unknown).
func (s *MessagesEnrichmentService) FillBodies(ctx context.Context, items []FillBodyItem) (*FillBodiesResult, error) {
	res := &FillBodiesResult{Results: make([]FillBodyResult, 0, len(items))}
	for _, item := range items {
		outcome, err := s.fillOne(ctx, item)
		if err != nil {
			return nil, fmt.Errorf("fill body %q: %w", item.Guid, err)
		}
		res.Results = append(res.Results, FillBodyResult{Guid: item.Guid, Outcome: outcome})
		switch outcome {
		case FillBodyFilled:
			res.Totals.Filled++
		case FillBodyUnchanged:
			res.Totals.Unchanged++
		case FillBodyUnknown:
			res.Totals.Unknown++
		}
	}
	return res, nil
}

func (s *MessagesEnrichmentService) fillOne(ctx context.Context, item FillBodyItem) (FillBodyOutcome, error) {
	filled, err := s.messages.FillBody(ctx, item.Guid, item.Body)
	if err != nil {
		return "", err
	}
	if filled {
		return FillBodyFilled, nil
	}
	// The row was not filled: it either already has a body or does not exist.
	if _, err := s.messages.GetMessage(ctx, item.Guid); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return FillBodyUnknown, nil
		}
		return "", err
	}
	return FillBodyUnchanged, nil
}
