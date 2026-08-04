package replay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
)

// Gate A seeded-sender predicates key on the SOURCE MESSAGE ROW's linkage to an
// interaction (by THIS replay's exact synthetic source id), which is both
// exact-to-this-replay (a prior same-contact interaction can't satisfy it) AND
// idempotent (re-replaying the same payload leaves the row linked, so the
// predicate stays true — a count-delta predicate would never re-fire). Each
// predicate is a closure over the relevant synthetic identifier(s); gcalSettled
// additionally closes over the contact id because the calendar_event predicate
// must confirm the contact landed in matched_contact_ids.

// gmailSettled returns a Gate A predicate: the comms_message(email, externalID)
// row is linked to an interaction.
func (h *Harness) gmailSettled(externalID string) gateA {
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountLinkedCommsMessageByExternalID(ctx, "email", externalID)
		return n > 0, err
	}
}

// gchatSettled returns a Gate A predicate: the comms_message(gchat, externalID)
// row is linked to an interaction.
func (h *Harness) gchatSettled(externalID string) gateA {
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountLinkedCommsMessageByExternalID(ctx, "gchat", externalID)
		return n > 0, err
	}
}

// whatsappSettled returns a Gate A predicate: the
// comms_message(whatsapp, externalID) row is linked to an interaction.
func (h *Harness) whatsappSettled(externalID string) gateA {
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountLinkedCommsMessageByExternalID(ctx, "whatsapp", externalID)
		return n > 0, err
	}
}

// imessageSettled returns a Gate A predicate: the messages_message(guid) row is
// linked to an interaction.
func (h *Harness) imessageSettled(guid string) gateA {
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountLinkedMessagesMessageByGuid(ctx, guid)
		return n > 0, err
	}
}

// telegramSettled returns a Gate A predicate: the telegram_message(peer, msgID)
// row is linked to an interaction. Scoped by peer (collision-checked at setup) in
// addition to message id.
func (h *Harness) telegramSettled(peerUserID int64, telegramMessageID int32) gateA {
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountLinkedTelegramMessageByMessageID(ctx, peerUserID, telegramMessageID)
		return n > 0, err
	}
}

// gcalSettled returns a Gate A predicate: the calendar_event(gcalID) row has the
// contact in matched_contact_ids and is processed (the attended interaction
// published).
func (h *Harness) gcalSettled(gcalEventID string, contactID uuid.UUID) gateA {
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountProcessedCalendarEventByGcalID(ctx, gcalEventID, contactID)
		return n > 0, err
	}
}

// assertContactVenue verifies that the contact has at least one interaction of
// the given source whose venue_id resolves to a live venue node. Called by the
// replay adapters that drive a venue-bearing recorder through the bus
// (telegram/messages/gchat/email/gcal) after settle, so a replay fails loudly if
// the venue-populating recorder path silently stopped setting venue_id. Returns
// an error (not a panic) so the adapter surfaces it like any other replay
// failure. expectedSource scopes the check to this replay's source so an
// unrelated prior interaction can't satisfy it.
//
// phone_calls + anarlog_sessions have NO replay adapter (they are ingest-driven
// via IngestService.handleCall / handleMeetingNoteRecorded, not a Replay* path),
// so their live venue population is instead covered by the ingest integration
// tests and the venue backfill integration test's per-source seeds.
func (h *Harness) assertContactVenue(ctx context.Context, contactID uuid.UUID, expectedSource string) error {
	rows, err := h.interactionRepo.ListContactInteractions(ctx, contactID, 100, 0)
	if err != nil {
		return fmt.Errorf("list interactions for venue assert: %w", err)
	}
	for _, r := range rows {
		if r.Source != expectedSource || r.VenueID == nil {
			continue
		}
		if _, err := h.venueRepo.GetVenue(ctx, *r.VenueID); err != nil {
			return fmt.Errorf("interaction %s venue %s not found: %w", r.ID, *r.VenueID, err)
		}
		return nil // found a source-matched interaction with a live venue
	}
	return fmt.Errorf("no %s interaction with a venue for contact %s", expectedSource, contactID)
}

// trackContactInteractions records the contact's interaction ids into the ledger
// so Cleanup deletes them by id (step 2) before the contact delete. It ALSO
// records each interaction's venue_id (the venue node the real recorder minted)
// so Cleanup can delete those venue nodes by id — they are not contacts and have
// an empty canonical_label, so neither the person-node nor the ns-prefix node
// delete catches them. Best-effort: a read error leaves the by-contact path to
// cover interactions; venue nodes whose interaction wasn't tracked are caught by
// no other path, which is why we track them here on the same scan.
func (h *Harness) trackContactInteractions(ctx context.Context, contactID uuid.UUID) {
	rows, err := h.interactionRepo.ListContactInteractions(ctx, contactID, 100, 0)
	if err != nil {
		return
	}
	h.track(func(c *created) {
		for _, r := range rows {
			c.addInteraction(r.ID)
			if r.VenueID != nil {
				c.addVenueNode(*r.VenueID)
			}
		}
	})
}

// gmailBackfillSince returns the Gmail scan-window floor for a message sent at
// sentAt: a couple of days before the message itself. The Gmail provider filters
// fetched messages by internalDate against the scan window [floor, safeHorizon]
// (the only source whose replay re-applies a time window), so a message replayed
// at ANY age — the interaction temporal spread replays the same contact across
// weeks/months — lands in the very first scan window regardless of how far back
// it is dated. The floor is day-granular (the Gmail metadata format) and UTC.
func gmailBackfillSince(sentAt time.Time) string {
	return sentAt.Add(-2 * 24 * time.Hour).UTC().Format("2006-01-02")
}

// telegramPeerStranded reports whether a telegram_message row exists for the
// peer with matched_contact_id IS NULL (the unknown-sender pending state).
func (h *Harness) telegramPeerStranded(ctx context.Context, peerUserID int64) (bool, error) {
	n, err := h.support.CountStrandedTelegramMessagesByPeer(ctx, peerUserID)
	return n > 0, err
}

// CommsRowExists reports whether a comms_message row exists for (source,
// external_id). Tests use it to assert the Gmail/GChat unknown-sender match-only
// contract (no row written for an unknown correspondent).
func (h *Harness) CommsRowExists(ctx context.Context, source, externalID string) (bool, error) {
	_, err := h.commsRepo.GetLatestByExternalID(ctx, source, externalID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
