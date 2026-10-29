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

// ListAllEffectiveParticipation returns every live contact's effective
// participations, read as one row per member and grouped here: a message
// burst is an entry of its own, and the members of one contact's root are
// adjacent.
func (r *InteractionRepository) ListAllEffectiveParticipation(ctx context.Context) ([]EffectiveParticipation, error) {
	rows, err := r.queries.ListAllEffectiveParticipationMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all effective participation: %w", err)
	}
	var out []EffectiveParticipation
	for _, row := range rows {
		last := len(out) - 1
		if last < 0 || row.RootNodeID == nil || out[last].ContactID != row.ContactID ||
			out[last].InteractionNodeID == nil || *out[last].InteractionNodeID != *row.RootNodeID {
			out = append(out, EffectiveParticipation{
				ContactID:         row.ContactID,
				InteractionNodeID: row.RootNodeID,
				Direction:         row.EntryDirection,
				OccurredAt:        row.EntryOccurredAt.UTC(),
			})
			last++
		}
		out[last].Members = append(out[last].Members, EffectiveParticipationMember{
			ObservationID:     row.ObservationID,
			Source:            row.Source,
			SourceRef:         row.SourceRef,
			InteractionNodeID: row.InteractionNodeID,
		})
	}
	return out, nil
}

// ListLiveObservationVenues maps every live observation recorded in a venue to
// that venue.
func (r *InteractionRepository) ListLiveObservationVenues(ctx context.Context) (map[uuid.UUID]uuid.UUID, error) {
	rows, err := r.queries.ListLiveObservationVenues(ctx)
	if err != nil {
		return nil, fmt.Errorf("list observation venues: %w", err)
	}
	venues := make(map[uuid.UUID]uuid.UUID, len(rows))
	for _, row := range rows {
		venues[row.ID] = row.VenueID
	}
	return venues, nil
}

// MessageCopy is one observation holding a copy of one message.
type MessageCopy struct {
	ObservationID uuid.UUID
	MessageKey    string
}

// ListGroupChatMessageCopies returns, for every group-chat message, each
// observation holding a copy of it.
func (r *InteractionRepository) ListGroupChatMessageCopies(ctx context.Context) ([]MessageCopy, error) {
	rows, err := r.queries.ListGroupChatMessageCopies(ctx)
	if err != nil {
		return nil, fmt.Errorf("list group chat message copies: %w", err)
	}
	copies := make([]MessageCopy, len(rows))
	for i, row := range rows {
		copies[i] = MessageCopy{ObservationID: row.ObservationID, MessageKey: row.MessageKey}
	}
	return copies, nil
}

// ThreadMessage is one message of a thread. ContactID is the contact a copy
// attributes it to: the sender of a received message.
type ThreadMessage struct {
	SentAt    time.Time
	FromUser  bool
	Body      string
	ContactID *uuid.UUID
}

// GetThreadMessage returns one message of a thread by its key, or
// db.ErrNotFound.
func (r *ThreadContentRepository) GetThreadMessage(ctx context.Context, venueID uuid.UUID, messageKey string) (*ThreadMessage, error) {
	row, err := r.queries.GetThreadMessage(ctx, db.GetThreadMessageParams{VenueID: venueID, MessageKey: messageKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, db.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get thread message: %w", err)
	}
	return &ThreadMessage{SentAt: row.SentAt, FromUser: row.FromUser, Body: row.Body, ContactID: row.PeerContactID}, nil
}
