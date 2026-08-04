package telegram

import (
	"context"
	"strconv"
	"strings"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// PostImportHook back-links a Telegram candidate's message history after the
// user imports or links it.
//
// It exists because the import handler's hook is now source-keyed and passes the
// whole external_contact row, while TelegramManager.OnPeerLinked keeps its
// existing (peerUserID, peerUsername) shape — it is also called by the rematch
// path. The adapter owns the interface and the field extraction; the manager
// method is untouched.
type PostImportHook struct {
	mgr *TelegramManager
}

// NewPostImportHook wraps a Telegram manager.
func NewPostImportHook(mgr *TelegramManager) *PostImportHook {
	return &PostImportHook{mgr: mgr}
}

// Source is the external_contact.source this hook handles.
func (h *PostImportHook) Source() string { return "telegram" }

// OnPeerLinked extracts the peer user id and handle from the candidate row and
// delegates. A source_id that is not a Telegram user id logs and returns nil,
// exactly as the handler did before the hook was source-keyed.
func (h *PostImportHook) OnPeerLinked(ctx context.Context, external *repository.ExternalContact, contactID uuid.UUID) error {
	if h.mgr == nil || external == nil {
		return nil
	}
	peerUserID, err := strconv.ParseInt(external.SourceID, 10, 64)
	if err != nil {
		log.Warn().Err(err).Msg("telegram: failed to parse peer user ID for post-import hook")
		return nil
	}
	var peerUsername string
	if username, ok := external.Metadata["username"].(string); ok {
		peerUsername = strings.TrimPrefix(username, "@")
	}
	return h.mgr.OnPeerLinked(ctx, peerUserID, peerUsername, contactID)
}
