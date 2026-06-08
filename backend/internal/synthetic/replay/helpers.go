package replay

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
)

// Gate A seeded-sender predicates key on the SOURCE MESSAGE ROW's linkage to an
// interaction (by THIS replay's exact synthetic source id), which is both
// exact-to-this-replay (a prior same-contact interaction can't satisfy it) AND
// idempotent (re-replaying the same payload leaves the row linked, so the
// predicate stays true — a count-delta predicate would never re-fire). The
// contactID arg is unused for message-linkage sources but kept in the signature
// for the gcal predicate that needs it.

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

// imessageSettled returns a Gate A predicate: the messages_message(guid) row is
// linked to an interaction.
func (h *Harness) imessageSettled(guid string) gateA {
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountLinkedMessagesMessageByGuid(ctx, guid)
		return n > 0, err
	}
}

// telegramSettled returns a Gate A predicate: the telegram_message(msgID) row is
// linked to an interaction.
func (h *Harness) telegramSettled(telegramMessageID int32) gateA {
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountLinkedTelegramMessageByMessageID(ctx, telegramMessageID)
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

// trackContactInteractions records the contact's interaction ids into the ledger
// so Cleanup deletes them by id (step 2) before the contact delete. Best-effort:
// a read error leaves the by-contact path to cover them.
func (h *Harness) trackContactInteractions(ctx context.Context, contactID uuid.UUID) {
	rows, err := h.interactionRepo.ListContactInteractions(ctx, contactID, 100, 0)
	if err != nil {
		return
	}
	h.track(func(c *created) {
		for _, r := range rows {
			c.addInteraction(r.ID)
		}
	})
}

// gmailBackfillSince returns a backfill floor far enough in the past that the
// synthetic message (anchored ~2h before the live anchor) is inside the scanned,
// already-closed Gmail window.
func gmailBackfillSince(h *Harness) string {
	return accelerated.GetCurrentTime().Add(-30 * 24 * time.Hour).UTC().Format("2006-01-02")
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
