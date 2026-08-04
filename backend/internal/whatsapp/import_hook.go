package whatsapp

import (
	"context"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// whatsappPeerLinker is the slice of *PeerMatcher this adapter delegates to.
//
// It is an interface rather than the concrete matcher so the EXTRACTION — the
// only logic this type owns — can be asserted against a recording fake. With a
// concrete dependency the sole reachable test call is a nil-constructed one,
// which cannot observe what was derived, so a wrong extraction would pass.
type whatsappPeerLinker interface {
	OnPeerLinked(ctx context.Context, peerJID string, phoneE164 *string, contactID uuid.UUID) error
}

// PostImportHook back-links a WhatsApp candidate's staged messages after the
// user imports or links it. It satisfies the import handler's hook interface
// structurally, so this package never imports the handler package.
type PostImportHook struct {
	matcher whatsappPeerLinker
}

// NewPostImportHook wraps the ingest path's peer matcher.
func NewPostImportHook(matcher whatsappPeerLinker) *PostImportHook {
	return &PostImportHook{matcher: matcher}
}

// Source is the external_contact.source this hook handles.
func (h *PostImportHook) Source() string { return syncSource }

// OnPeerLinked reads the peer JID off the candidate's source_id — which is the
// RAW peer JID, the same value comms_message.peer_handle carries — and the
// resolved phone off its metadata, then attaches the staged messages.
//
// The extraction runs BEFORE the delegate nil-check, so every call exercises
// it rather than short-circuiting past the only logic this type has.
func (h *PostImportHook) OnPeerLinked(ctx context.Context, external *repository.ExternalContact, contactID uuid.UUID) error {
	if external == nil {
		return nil
	}
	var phone *string
	if v, ok := external.Metadata["phone_e164"].(string); ok && v != "" {
		phone = &v
	}
	if h.matcher == nil {
		return nil
	}
	return h.matcher.OnPeerLinked(ctx, external.SourceID, phone, contactID)
}
