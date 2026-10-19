package judgment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// runRow reads a run, or returns ErrRunNotFound.
func (s *Store) runRow(ctx context.Context, id RunID) (*db.JudgmentRun, error) {
	run, err := s.queries.GetJudgmentRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", id, err)
	}
	return run, nil
}

// allContent is a cut-off after every message.
var allContent = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

// threadMessages are a thread's messages once, in thread order, and the names
// of the contacts they are from.
type threadMessages struct {
	list  []ThreadMessage
	names map[uuid.UUID]string
}

// messages reads the thread's whole conversation, oldest first, from the one
// definition of a thread's content its judgments read: each message once
// however many per-contact copies hold it, attributed to the contact any copy
// names. A meeting's notes are its messages, from the user.
func (s *Store) threadMessages(ctx context.Context, thread uuid.UUID) (*threadMessages, error) {
	rows, err := repository.NewThreadContentRepository(s.queries).ListThreadContent(ctx, thread, allContent)
	if err != nil {
		return nil, fmt.Errorf("list thread content: %w", err)
	}
	out := &threadMessages{list: []ThreadMessage{}}
	seen := map[string]bool{}
	var named []uuid.UUID
	for _, row := range rows {
		// Copies sort with an attributed one first, so the first copy names the
		// message's contact when any copy does.
		if seen[row.MessageKey] {
			continue
		}
		seen[row.MessageKey] = true
		m := ThreadMessage{Key: row.MessageKey, SentAt: row.SentAt, FromUser: row.FromUser, Text: row.Body}
		if !row.FromUser {
			m.Sender = row.PeerContactID
		}
		named = appendContact(named, nil, &m)
		out.list = append(out.list, m)
	}
	if out.names, err = s.contacts.ListContactNamesByIDs(ctx, named); err != nil {
		return nil, fmt.Errorf("list contact names: %w", err)
	}
	return out, nil
}

// review compiles the thread's review state in the run, with each night's
// reply proposal and the closest unflagged messages' senders.
func (s *Store) reviewThread(ctx context.Context, run *db.JudgmentRun, thread uuid.UUID, messages *threadMessages) (*ThreadReview, error) {
	data, err := s.loadReviewData(ctx, run, []uuid.UUID{thread})
	if err != nil {
		return nil, err
	}
	review, err := data.build(ctx, thread)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]ThreadMessage, len(messages.list))
	for _, m := range messages.list {
		byKey[m.Key] = m
	}
	rows := make([]*db.ListJudgmentRowsRow, 0, len(review.Nights))
	for _, n := range review.Nights {
		if !slices.ContainsFunc(rows, func(r *db.ListJudgmentRowsRow) bool { return r.ID == n.JudgmentID }) {
			rows = append(rows, data.rows[n.JudgmentID])
		}
	}
	proposals, err := s.threadProposals(ctx, thread, rows)
	if err != nil {
		return nil, err
	}
	review.Names = map[uuid.UUID]string{}
	for i := range review.Nights {
		n := &review.Nights[i]
		at := slices.IndexFunc(proposals, func(p Proposal) bool { return p.JudgmentID == n.JudgmentID })
		n.Proposal = &proposals[at]
		for id, name := range proposals[at].Names {
			review.Names[id] = name
		}
		for ci, c := range n.Closest {
			if c.Message != nil {
				n.Closest[ci].Message = ptrTo(byKey[c.Message.Key])
			}
		}
	}
	for id, name := range messages.names {
		review.Names[id] = name
	}
	return review, nil
}

func ptrTo[T any](v T) *T { return &v }

// reviewStateOf compiles the thread's review state in the run, reading the
// thread's messages for the senders it names.
func (s *Store) reviewStateOf(ctx context.Context, run *db.JudgmentRun, thread uuid.UUID) (*ThreadReview, error) {
	messages, err := s.threadMessages(ctx, thread)
	if err != nil {
		return nil, err
	}
	return s.reviewThread(ctx, run, thread, messages)
}

// ReviewStateOfJudgment returns the review state, in the run, of the thread the
// judgment judged. It returns ErrNotFound for an unknown judgment and
// ErrRunNotFound for an unknown run.
func (s *Store) ReviewStateOfJudgment(ctx context.Context, run RunID, judgment uuid.UUID) (*ThreadReview, error) {
	runRow, err := s.runRow(ctx, run)
	if err != nil {
		return nil, err
	}
	rows, err := s.queries.ListJudgmentRows(ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{judgment}, ExampleIds: []string{}})
	if err != nil {
		return nil, fmt.Errorf("list judgments: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return s.reviewStateOf(ctx, runRow, rows[0].ThreadID)
}
