package whatsapp

import (
	"context"
	"fmt"
	"strings"

	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// commsAttacher is the slice of *repository.CommsMessageRepository the rematch
// handlers write through. Narrow so unit tests need no pool.
type commsAttacher interface {
	AttachUnmatchedByPeer(ctx context.Context, source string, peerNormalized, peerHandle *string, contactID uuid.UUID) (int64, int64, error)
}

// contactAggregator is the slice of *aggregation.Engine the handlers drive
// after a successful attach.
type contactAggregator interface {
	AggregateForContactBatch(ctx context.Context, contactID uuid.UUID) error
}

// phoneRematchBase holds the shared dependencies of the two WhatsApp rematch
// handlers, which differ only in the contact-method type they bind to.
//
// Both attach by peer_normalized in ONE repository call rather than enumerating
// peers: a RematchHandler receives the newly-added contact method's NORMALIZED
// value, which for both 'phone' and 'whatsapp' methods is an E.164 string —
// there is no peer JID to hand PeerMatcher.OnPeerLinked. Attaching by the number
// also reaches the case where one human is staged under two handles (a PN-form
// JID and a LID-form JID) that both resolved to the same phone.
//
// No identity row is minted for the peer here, deliberately: the WhatsApp
// identifier type dual-maps to [whatsapp, phone], so the peer's next live
// message matches through the newly-created contact method and runs the ingest
// path's own reconciliation (including the import-candidate status).
type phoneRematchBase struct {
	commsRepo commsAttacher
	engine    contactAggregator
}

// rematchByPhone attaches every unmatched WhatsApp row staged under this E.164
// number to the contact, then derives the interactions in the same pass.
//
// An attach failure is RETURNED rather than swallowed: the rematch dispatcher's
// River job is the retry, and a silently-swallowed attach leaves the messages
// invisible with nothing reporting why.
func (b *phoneRematchBase) rematchByPhone(ctx context.Context, contactID uuid.UUID, e164 string) (int, error) {
	e164 = strings.TrimSpace(e164)
	if e164 == "" {
		return 0, nil
	}

	attached, deduped, err := b.commsRepo.AttachUnmatchedByPeer(
		ctx,
		repository.InteractionSourceWhatsApp,
		&e164, // peer_normalized: the E.164 the ingest path staged
		nil,   // peer_handle: a contact method carries no peer JID
		contactID,
	)
	if err != nil {
		return 0, fmt.Errorf("attach whatsapp staged messages: %w", err)
	}
	if attached == 0 {
		return 0, nil
	}

	// The phone number is deliberately absent: an identifier is the part of a
	// third party's data that is legible on sight, and the counts are what
	// triage actually needs. The contact id attributes the attach, matching
	// Telegram's rematch logging.
	logger.Info().
		Str("contact_id", contactID.String()).
		Int64("attached", attached).
		Int64("deduped", deduped).
		Msg("whatsapp rematch: attached staged messages")

	if aggErr := b.engine.AggregateForContactBatch(ctx, contactID); aggErr != nil {
		// The rows ARE attached, so the count is real even when aggregation
		// fails; the next sweeper tick retries the aggregation.
		return int(attached), fmt.Errorf("aggregate for contact: %w", aggErr)
	}
	return int(attached), nil
}

// WhatsAppMethodRematchHandler implements service.RematchHandler for the
// "whatsapp" contact-method type.
type WhatsAppMethodRematchHandler struct{ phoneRematchBase }

// NewWhatsAppMethodRematchHandler constructs the whatsapp-method rematch handler.
func NewWhatsAppMethodRematchHandler(commsRepo commsAttacher, engine contactAggregator) *WhatsAppMethodRematchHandler {
	return &WhatsAppMethodRematchHandler{phoneRematchBase{commsRepo: commsRepo, engine: engine}}
}

// IdentifierType returns the contact_method type this handler binds to.
func (h *WhatsAppMethodRematchHandler) IdentifierType() string {
	return string(repository.ContactMethodWhatsApp)
}

// Rematch attaches the staged WhatsApp messages for the newly-added whatsapp
// method and derives their interactions.
func (h *WhatsAppMethodRematchHandler) Rematch(ctx context.Context, contactID uuid.UUID, valueNormalized string) (int, error) {
	return h.rematchByPhone(ctx, contactID, valueNormalized)
}

// PhoneRematchHandler implements service.RematchHandler for the "phone"
// contact-method type. It co-registers alongside Telegram's phone handler; each
// reports its own count over its own source's rows.
type PhoneRematchHandler struct{ phoneRematchBase }

// NewPhoneRematchHandler constructs the phone rematch handler.
func NewPhoneRematchHandler(commsRepo commsAttacher, engine contactAggregator) *PhoneRematchHandler {
	return &PhoneRematchHandler{phoneRematchBase{commsRepo: commsRepo, engine: engine}}
}

// IdentifierType returns the contact_method type this handler binds to.
func (h *PhoneRematchHandler) IdentifierType() string {
	return string(repository.ContactMethodPhone)
}

// Rematch attaches the staged WhatsApp messages for the newly-added phone
// number and derives their interactions. WhatsApp peers are identified by phone
// number, so a plain phone method is a valid WhatsApp identity.
func (h *PhoneRematchHandler) Rematch(ctx context.Context, contactID uuid.UUID, valueNormalized string) (int, error) {
	return h.rematchByPhone(ctx, contactID, valueNormalized)
}
