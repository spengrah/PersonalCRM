package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/whatsapp"

	"github.com/google/uuid"
)

// whatsappDeps bundles the WhatsApp ingest seam the adapter drives. It mirrors
// telegramPeerMatcherDeps: per-source deps are Harness fields, since the package
// has no adapter registry.
type whatsappDeps struct {
	ingestor *whatsapp.Ingestor
	// discoveryMinMessages is the threshold the ingestor's OWN PeerMatcher was
	// built with (the same value, not a second reading of the config), so a
	// caller sizing an unmatched-peer fixture can ask for the count that
	// actually mints a candidate rather than restating the default.
	discoveryMinMessages int
}

// WhatsAppDiscoveryMinMessages is how many unmatched messages one peer needs
// before the ingest path mints an import candidate for it. Exposed for the same
// reason GroupMaxMembers is: a fixture that wants the discovered state must size
// itself against the real threshold, never a magic number.
func (h *Harness) WhatsAppDiscoveryMinMessages() int {
	if h.whatsapp == nil {
		return 0
	}
	return h.whatsapp.discoveryMinMessages
}

// WhatsAppResult is the settled outcome of a WhatsApp replay.
type WhatsAppResult struct {
	ContactID uuid.UUID
	PeerJID   string
	Matched   bool // false for MatchUnknown (the row IS written, just unattached)
}

// ReplayWhatsApp feeds a synthetic message through the REAL Ingestor — the same
// seam the live handler and the history drainer use — so a seeded row is
// byte-identical in shape to a production one.
//
// WhatsApp differs from Gmail/GChat on the unknown-sender path: an unrecognised
// peer still WRITES a comms_message row (matched_contact_id IS NULL is legal for
// this source), it just never aggregates. So MatchUnknown is a PENDING outcome
// here, not a match-only one.
//
// contactID is the seeded contact this message targets (for MatchSeeded). The
// caller must seed it with a phone or whatsapp method matching the peer's
// number.
func (h *Harness) ReplayWhatsApp(ctx context.Context, contactID uuid.UUID, spec factory.WhatsAppMessageSpec) (WhatsAppResult, error) {
	if h.whatsapp == nil {
		return WhatsAppResult{}, fmt.Errorf("whatsapp: harness has no whatsapp deps")
	}

	body := spec.Body
	pushName := spec.PushName
	peerJID := spec.PeerJID
	peerPhone := spec.PeerPhone
	accountJID := spec.AccountJID

	msg := whatsapp.IngestedMessage{
		MessageID:     spec.ExternalID,
		ChatJID:       spec.ChatJID,
		ChatType:      whatsapp.ChatTypePrivate,
		IsOutgoing:    spec.Outbound,
		SentAt:        spec.SentAt,
		Body:          &body,
		MessageType:   whatsapp.MessageTypeText,
		PeerJID:       &peerJID,
		PeerPhoneE164: &peerPhone,
		PushName:      &pushName,
		AccountJID:    &accountJID,
	}

	// The real seam. Synchronous: the staged row is durable when it returns.
	if err := h.whatsapp.ingestor.IngestMessage(ctx, msg); err != nil {
		return WhatsAppResult{}, fmt.Errorf("whatsapp ingest: %w", err)
	}

	// An unmatched peer that crosses the discovery threshold mints an
	// external_contact candidate whose source_id is a bare JID — the ns-prefix
	// cleanup step cannot reach it, so track it by id.
	if external, err := h.externalRepo.GetBySource(ctx, repository.InteractionSourceWhatsApp, spec.PeerJID, nil); err == nil && external != nil {
		h.track(func(c *created) { c.addExternalContact(external.ID) })
	}

	if spec.Intent == factory.MatchUnknown {
		// Pending: the row exists but is unattached, so it never aggregates and
		// there is no async work to wait for. Settle on the trivially-true gate
		// so the harness's settle bookkeeping stays uniform across adapters.
		if err := h.Settle(ctx, func(context.Context) (bool, error) { return true, nil }, ""); err != nil {
			return WhatsAppResult{}, err
		}
		return WhatsAppResult{PeerJID: spec.PeerJID, Matched: false}, nil
	}

	// Drive aggregation for the matched contact through the WORKER path (the
	// ingestor writes the row and publishes nothing; the engine derives the
	// interaction on its own pass).
	if _, err := h.client.Insert(ctx, consumerjobs.MessagingAggregateForContactArgs{
		ContactID: contactID,
		Source:    repository.InteractionSourceWhatsApp,
	}, nil); err != nil {
		return WhatsAppResult{}, fmt.Errorf("whatsapp enqueue aggregate: %w", err)
	}

	// Gate A: THIS replay's whatsapp row is linked to an interaction.
	if err := h.Settle(ctx, h.whatsappSettled(spec.ExternalID), repository.InteractionSourceWhatsApp); err != nil {
		return WhatsAppResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceWhatsApp); err != nil {
		return WhatsAppResult{}, err
	}
	return WhatsAppResult{ContactID: contactID, PeerJID: spec.PeerJID, Matched: true}, nil
}
