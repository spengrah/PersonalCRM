package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ThreadContentRow is one stored copy of one message (or meeting note) of a
// thread, read from the thread_content view. Copies of one message share
// MessageKey.
type ThreadContentRow struct {
	VenueKind  string
	MessageKey string
	SentAt     time.Time
	// FromUser is true for the user's own message and for a meeting note.
	FromUser bool
	// PeerKey is the source identity of the copy's peer: the sender of a
	// received message, the recipient of the user's own. Empty when the source
	// names no recipient.
	PeerKey string
	// PeerContactID is the live contact the copy is attributed to: the sender
	// of a received message, a recipient of the user's own.
	PeerContactID *uuid.UUID
	// PeerAliases are the names and handles the source gives the copy's peer.
	PeerAliases []string
	// UserAliases are the names and handles the source gives the user on the
	// user's own message.
	UserAliases []string
	Body        string
	Headers     []byte
	// Reaction is the facts of a copy that is a reaction, nil on any other.
	// Its TargetKey is in MessageKey form, so it equals the target's MessageKey.
	Reaction *MessageReaction
}

// ContactName is one name the CRM holds for a live contact.
type ContactName struct {
	ContactID uuid.UUID
	Name      string
}

// ThreadContentRepository reads thread content through the thread_content view.
type ThreadContentRepository struct {
	queries db.Querier
}

// NewThreadContentRepository builds the repository.
func NewThreadContentRepository(queries db.Querier) *ThreadContentRepository {
	return &ThreadContentRepository{queries: queries}
}

// WithTx returns a repository bound to the transaction, so a caller inside a
// transaction reads the content that transaction wrote.
func (r *ThreadContentRepository) WithTx(tx pgx.Tx) *ThreadContentRepository {
	return &ThreadContentRepository{queries: db.New(tx)}
}

// ListThreadContent returns every stored copy of the thread's content at or
// before cutOff, ordered by time.
func (r *ThreadContentRepository) ListThreadContent(ctx context.Context, venueID uuid.UUID, cutOff time.Time) ([]ThreadContentRow, error) {
	rows, err := r.queries.ListThreadContent(ctx, db.ListThreadContentParams{VenueID: venueID, CutOff: cutOff})
	if err != nil {
		return nil, err
	}
	out := make([]ThreadContentRow, len(rows))
	for i, row := range rows {
		out[i] = ThreadContentRow{VenueKind: row.VenueKind, MessageKey: row.MessageKey, SentAt: row.SentAt, FromUser: row.FromUser, PeerKey: row.PeerKey, PeerContactID: row.PeerContactID, PeerAliases: row.PeerAliases, UserAliases: row.UserAliases, Body: row.Body, Headers: row.Headers,
			Reaction: reactionFromColumns(row.ReactionKind, row.ReactionEmoji, row.ReactionTargetKey, row.ReactionRemoval)}
	}
	return out, nil
}

// LatestThreadContentAt returns the time of the thread's latest content at or
// before asOf, and false when it has none.
func (r *ThreadContentRepository) LatestThreadContentAt(ctx context.Context, venueID uuid.UUID, asOf time.Time) (time.Time, bool, error) {
	latest, err := r.queries.GetLatestThreadContentAt(ctx, db.GetLatestThreadContentAtParams{VenueID: venueID, AsOf: asOf})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return latest, true, nil
}

// ListThreadsWithContentIn returns the threads with content after after, up to
// and including upTo.
func (r *ThreadContentRepository) ListThreadsWithContentIn(ctx context.Context, after, upTo time.Time) ([]uuid.UUID, error) {
	return r.queries.ListThreadsWithContentIn(ctx, db.ListThreadsWithContentInParams{After: after, UpTo: upTo})
}

// ListVenueContactsAsOf returns the live contacts with a live interaction in the
// venue at or before asOf.
func (r *ThreadContentRepository) ListVenueContactsAsOf(ctx context.Context, venueID uuid.UUID, asOf time.Time) ([]uuid.UUID, error) {
	return r.queries.ListVenueContactsAsOf(ctx, db.ListVenueContactsAsOfParams{VenueID: venueID, AsOf: asOf})
}

// ListContactNamesForMasking returns every name the CRM holds for each live
// contact.
func (r *ThreadContentRepository) ListContactNamesForMasking(ctx context.Context) ([]ContactName, error) {
	rows, err := r.queries.ListContactNamesForMasking(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ContactName, len(rows))
	for i, row := range rows {
		out[i] = ContactName{ContactID: row.ContactID, Name: row.Name}
	}
	return out, nil
}
