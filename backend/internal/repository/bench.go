package repository

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListAllEffectiveParticipation returns every live contact's effective
// participations. It reads the derivation's member rows
// (effective_participation_member) and packs each entry's rows, in member
// order, as the effective_participation view packs them, without its JSON.
func (r *InteractionRepository) ListAllEffectiveParticipation(ctx context.Context) ([]EffectiveParticipation, error) {
	rows, err := r.queries.ListAllEffectiveParticipationMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all effective participation: %w", err)
	}
	slices.SortFunc(rows, func(a, b *db.ListAllEffectiveParticipationMembersRow) int {
		return cmp.Or(
			bytes.Compare(a.ContactID[:], b.ContactID[:]),
			bytes.Compare(a.EntryID[:], b.EntryID[:]),
			cmp.Compare(a.MemberOrdinal, b.MemberOrdinal),
		)
	})
	var out []EffectiveParticipation
	for i, row := range rows {
		if i == 0 || row.ContactID != rows[i-1].ContactID || row.EntryID != rows[i-1].EntryID {
			out = append(out, EffectiveParticipation{
				ContactID:         row.ContactID,
				InteractionNodeID: row.RootNodeID,
				Direction:         row.EntryDirection,
				OccurredAt:        row.EntryOccurredAt.UTC(),
			})
		}
		last := len(out) - 1
		out[last].Members = append(out[last].Members, EffectiveParticipationMember{
			ObservationID:     row.ObservationID,
			Source:            row.Source,
			SourceRef:         row.SourceRef,
			InteractionNodeID: row.InteractionNodeID,
		})
	}
	return out, nil
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
