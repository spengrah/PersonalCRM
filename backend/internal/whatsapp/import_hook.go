package whatsapp

import (
	"context"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// PostImportHook back-links a WhatsApp candidate's staged messages after the
// user imports or links it. It satisfies the import handler's hook interface
// structurally, so this package never imports the handler package.
type PostImportHook struct {
	matcher *PeerMatcher
}

// NewPostImportHook wraps the ingest path's peer matcher.
func NewPostImportHook(matcher *PeerMatcher) *PostImportHook {
	return &PostImportHook{matcher: matcher}
}

// Source is the external_contact.source this hook handles.
func (h *PostImportHook) Source() string { return syncSource }

// OnPeerLinked reads the peer JID off the candidate's source_id — which is the
// RAW peer JID, the same value comms_message.peer_handle carries — and the
// resolved phone off its metadata, then attaches the staged messages.
func (h *PostImportHook) OnPeerLinked(ctx context.Context, external *repository.ExternalContact, contactID uuid.UUID) error {
	if h.matcher == nil || external == nil {
		return nil
	}
	var phone *string
	if v, ok := external.Metadata["phone_e164"].(string); ok && v != "" {
		phone = &v
	}
	return h.matcher.OnPeerLinked(ctx, external.SourceID, phone, contactID)
}
