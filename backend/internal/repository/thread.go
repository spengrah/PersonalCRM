package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ThreadSummary is one thread holding content: its venue, the time of its
// latest message and its count of messages, a meeting's notes counted as its
// messages.
type ThreadSummary struct {
	ID           uuid.UUID
	VenueKind    string
	Source       string
	Title        *string
	MessageCount int
	LatestAt     time.Time
}

// ThreadFilter selects threads. Sources keeps the venues of those sources (all
// when empty) and ContactID the threads that contact takes part in. With
// Restrict set, only the threads Only names are kept, so an empty Only keeps
// none.
type ThreadFilter struct {
	Sources   []string
	ContactID *uuid.UUID
	Restrict  bool
	Only      []uuid.UUID
}

// ThreadParticipant is a live contact that takes part in a thread.
type ThreadParticipant struct {
	ContactID uuid.UUID
	Name      string
}

// ThreadMessageRef names one message of a thread in thread order.
type ThreadMessageRef struct {
	Key    string
	SentAt time.Time
}

// ThreadRepository reads the threads of the Threads tab.
type ThreadRepository struct {
	queries db.Querier
}

// NewThreadRepository builds the repository.
func NewThreadRepository(queries db.Querier) *ThreadRepository {
	return &ThreadRepository{queries: queries}
}

// ListThreads returns every thread the filter keeps, by latest message, newest
// first, ties by thread ID descending.
func (r *ThreadRepository) ListThreads(ctx context.Context, f ThreadFilter) ([]ThreadSummary, error) {
	rows, err := r.queries.ListThreads(ctx, db.ListThreadsParams{
		Sources: nonNilStrings(f.Sources), OnlyIds: f.Restrict, VenueIds: nonNilUUIDs(f.Only), ContactID: f.ContactID,
	})
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	out := make([]ThreadSummary, len(rows))
	for i, row := range rows {
		out[i] = ThreadSummary{
			ID: row.VenueID, VenueKind: row.Kind, Source: row.Source, Title: row.Title,
			MessageCount: int(row.MessageCount), LatestAt: row.LatestAt,
		}
	}
	return out, nil
}

// GetThread returns the thread holding content, or db.ErrNotFound.
func (r *ThreadRepository) GetThread(ctx context.Context, id uuid.UUID) (*ThreadSummary, error) {
	row, err := r.queries.GetThread(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, db.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get thread: %w", err)
	}
	return &ThreadSummary{
		ID: row.VenueID, VenueKind: row.Kind, Source: row.Source, Title: row.Title,
		MessageCount: int(row.MessageCount), LatestAt: row.LatestAt,
	}, nil
}

// HoldsMessage reports whether the thread holds a message with the key.
func (r *ThreadRepository) HoldsMessage(ctx context.Context, id uuid.UUID, messageKey string) (bool, error) {
	held, err := r.queries.ThreadHoldsMessage(ctx, db.ThreadHoldsMessageParams{VenueID: id, MessageKey: messageKey})
	if err != nil {
		return false, fmt.Errorf("thread holds message: %w", err)
	}
	return held, nil
}

// ListThreadParticipants returns the live contacts taking part in each given
// thread, by name, then contact ID.
func (r *ThreadRepository) ListThreadParticipants(ctx context.Context, threads []uuid.UUID) (map[uuid.UUID][]ThreadParticipant, error) {
	rows, err := r.queries.ListThreadParticipants(ctx, nonNilUUIDs(threads))
	if err != nil {
		return nil, fmt.Errorf("list thread participants: %w", err)
	}
	out := map[uuid.UUID][]ThreadParticipant{}
	for _, row := range rows {
		out[row.VenueID] = append(out[row.VenueID], ThreadParticipant{ContactID: row.ParticipantID, Name: row.ParticipantName})
	}
	return out, nil
}

// ListThreadMessageRefs returns each message of the given threads once, in
// thread order: by time, then message key.
func (r *ThreadRepository) ListThreadMessageRefs(ctx context.Context, threads []uuid.UUID) (map[uuid.UUID][]ThreadMessageRef, error) {
	rows, err := r.queries.ListThreadMessageKeys(ctx, nonNilUUIDs(threads))
	if err != nil {
		return nil, fmt.Errorf("list thread message keys: %w", err)
	}
	out := map[uuid.UUID][]ThreadMessageRef{}
	for _, row := range rows {
		out[row.VenueID] = append(out[row.VenueID], ThreadMessageRef{Key: row.MessageKey, SentAt: row.SentAt})
	}
	return out, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilUUIDs(s []uuid.UUID) []uuid.UUID {
	if s == nil {
		return []uuid.UUID{}
	}
	return s
}
