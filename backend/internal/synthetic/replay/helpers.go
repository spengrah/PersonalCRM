package replay

import (
	"context"
	"time"

	"personal-crm/backend/internal/accelerated"

	"github.com/google/uuid"
)

// contactHasInteractionSource reports whether the contact has at least one
// interaction with the given source. Because every replay seeds a FRESH
// namespaced contact, "an interaction with this source for this contact" is
// exact-to-this-replay (a prior run's row belongs to a different contact id).
func (h *Harness) contactHasInteractionSource(ctx context.Context, contactID uuid.UUID, source string) (bool, error) {
	rows, err := h.interactionRepo.ListContactInteractions(ctx, contactID, 100, 0)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Source == source {
			return true, nil
		}
	}
	return false, nil
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
