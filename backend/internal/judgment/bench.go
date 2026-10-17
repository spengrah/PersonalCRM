package judgment

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
)

// Bench reads the all-interactions view: every interaction across contacts,
// and on each row the proposals of the thread judgments whose latest content
// the row holds.
type Bench struct {
	store        *Store
	content      *service.InteractionContentService
	interactions *repository.InteractionRepository
	threads      *repository.ThreadContentRepository
}

// NewBench builds the bench's read over the judgment store and the
// interaction content service.
func NewBench(store *Store, content *service.InteractionContentService) *Bench {
	return &Bench{
		store:        store,
		content:      content,
		interactions: repository.NewInteractionRepository(store.queries),
		threads:      repository.NewThreadContentRepository(store.queries),
	}
}

// BenchQuery selects a page of the view. Run narrows it to the rows a
// judgment appearing in the run is placed on (anchorRow), and each row's
// proposals to the judgments appearing in it.
type BenchQuery struct {
	ContactID *uuid.UUID
	Type      *service.InteractionType
	Run       *RunID
	Limit     int32
	Offset    int32
}

// BenchRow is one row with the proposals it carries, the production method's
// first and then by method key, and how many of their items await review.
type BenchRow struct {
	service.InteractionRow
	Proposals []Proposal
	ToReview  int
}

// BenchPage is one page of rows and the number of rows the query keeps.
type BenchPage struct {
	Rows  []BenchRow
	Total int
}

// List returns one page of the view.
func (b *Bench) List(ctx context.Context, q BenchQuery) (*BenchPage, error) {
	idx, err := b.content.IndexInteractions(ctx)
	if err != nil {
		return nil, err
	}
	filter := service.InteractionsFilter{ContactID: q.ContactID, Type: q.Type, Limit: q.Limit, Offset: q.Offset}
	if q.Run != nil {
		judged, err := b.store.ThreadsJudgedInRun(ctx, *q.Run)
		if err != nil {
			return nil, err
		}
		filter.Rows = make(map[service.TimelineIdentity]struct{}, len(judged))
		for _, t := range judged {
			row, ok, err := b.anchorRow(ctx, idx.RowOf, t.Thread, t.CutOff)
			if err != nil {
				return nil, err
			}
			if ok {
				filter.Rows[row] = struct{}{}
			}
		}
	}
	page, err := b.content.ListInteractions(ctx, idx, filter)
	if err != nil {
		return nil, err
	}
	rows := make([]BenchRow, len(page.Rows))
	onPage := map[service.TimelineIdentity]int{}
	var threads []uuid.UUID
	for i, row := range page.Rows {
		rows[i] = BenchRow{InteractionRow: row, Proposals: []Proposal{}}
		onPage[row.Identity] = i
		threads = append(threads, row.Threads...)
	}
	slices.SortFunc(threads, func(a, b uuid.UUID) int { return cmp.Compare(a.String(), b.String()) })
	for _, thread := range slices.Compact(threads) {
		proposals, err := b.store.ThreadProposals(ctx, thread, q.Run)
		if err != nil {
			return nil, err
		}
		for _, p := range proposals {
			row, ok, err := b.anchorRow(ctx, idx.RowOf, thread, p.CutOff)
			if err != nil {
				return nil, err
			}
			if i, here := onPage[row]; ok && here {
				rows[i].Proposals = append(rows[i].Proposals, p)
			}
		}
	}
	for i := range rows {
		slices.SortFunc(rows[i].Proposals, func(a, b Proposal) int {
			if a.Production != b.Production {
				if a.Production {
					return -1
				}
				return 1
			}
			return cmp.Or(cmp.Compare(a.Method.Key, b.Method.Key), cmp.Compare(a.Method.Version, b.Method.Version), a.CutOff.Compare(b.CutOff))
		})
		for _, p := range rows[i].Proposals {
			rows[i].ToReview += p.ToReview
		}
	}
	return &BenchPage{Rows: rows, Total: page.Total}, nil
}

// ThreadContent is a judged thread's messages and the names of the contacts
// they are from.
type ThreadContent struct {
	Messages []ThreadMessage
	Names    map[uuid.UUID]string
}

// allContent is a cut-off after every message.
var allContent = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

// ThreadContent returns the thread's whole conversation, oldest first, read
// from the one definition of a thread's content that its judgments read: each
// message once however many per-contact copies hold it, attributed to the
// contact any copy names. The content before a judgment's cut-off is what the
// judgment read; the rest came after it. A meeting's notes are its row's
// content, not messages.
func (b *Bench) ThreadContent(ctx context.Context, thread uuid.UUID) (*ThreadContent, error) {
	rows, err := b.threads.ListThreadContent(ctx, thread, allContent)
	if err != nil {
		return nil, fmt.Errorf("list thread content: %w", err)
	}
	content := &ThreadContent{Messages: []ThreadMessage{}}
	seen := map[string]bool{}
	var named []uuid.UUID
	for _, row := range rows {
		// Copies sort with an attributed one first, so the first copy names the
		// message's contact when any copy does.
		if seen[row.MessageKey] || !slices.Contains(conversationVenues, row.VenueKind) {
			continue
		}
		seen[row.MessageKey] = true
		m := ThreadMessage{Key: row.MessageKey, SentAt: row.SentAt, FromUser: row.FromUser, Text: row.Body}
		if !row.FromUser {
			m.Sender = row.PeerContactID
			named = appendContact(named, nil, &m)
		}
		content.Messages = append(content.Messages, m)
	}
	if content.Names, err = b.store.contacts.ListContactNamesByIDs(ctx, named); err != nil {
		return nil, fmt.Errorf("list contact names: %w", err)
	}
	return content, nil
}

// conversationVenues are the venue kinds whose content is messages.
var conversationVenues = []string{repository.VenueKindDM, repository.VenueKindGroupChat, repository.VenueKindEmailThread}

// anchorRow is the row a judgment of the thread at cutOff appears on: the row
// holding the thread's latest content at or before the cut-off; for a thread
// whose content no row holds (a meeting), the row of its latest observation at
// or before the cut-off, else of its earliest. It returns false when no row
// holds any of the thread's observations.
func (b *Bench) anchorRow(ctx context.Context, rowOf map[uuid.UUID]service.TimelineIdentity, thread uuid.UUID, cutOff time.Time) (service.TimelineIdentity, bool, error) {
	anchors, err := b.threads.ListThreadAnchorObservations(ctx, thread, cutOff)
	if err != nil {
		return service.TimelineIdentity{}, false, err
	}
	for _, id := range anchors {
		if row, ok := rowOf[id]; ok {
			return row, true, nil
		}
	}
	observations, err := b.interactions.ListVenueObservations(ctx, thread)
	if err != nil {
		return service.TimelineIdentity{}, false, fmt.Errorf("anchor row: %w", err)
	}
	var latest, earliest *service.TimelineIdentity
	for _, o := range observations {
		row, ok := rowOf[o.ID]
		if !ok {
			continue
		}
		if earliest == nil {
			earliest = &row
		}
		if !o.OccurredAt.After(cutOff) {
			latest = &row
		}
	}
	switch {
	case latest != nil:
		return *latest, true, nil
	case earliest != nil:
		return *earliest, true, nil
	default:
		return service.TimelineIdentity{}, false, nil
	}
}
