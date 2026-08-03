package whatsapp

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// notificationRecorder is the slice of repository.WhatsAppRepository the
// recorder writes through. The returned row id is the drainer's concern, not
// the recorder's.
type notificationRecorder interface {
	RecordNotification(ctx context.Context, protocolMsgID string, notification []byte, syncType string, chunkOrder int32, oldestMsgTS *time.Time, disposition string) (uuid.UUID, error)
}

// HistoryRecorder is the production HistoryNotificationRecorder: one INSERT and
// nothing else. It never downloads, projects, acks, or deletes, and it stores a
// media pointer rather than message content.
type HistoryRecorder struct {
	repo notificationRecorder
}

var _ HistoryNotificationRecorder = (*HistoryRecorder)(nil)

// NewHistoryRecorder builds the recorder over the WhatsApp repository.
func NewHistoryRecorder(repo notificationRecorder) *HistoryRecorder {
	return &HistoryRecorder{repo: repo}
}

// RecordHistoryNotification persists one outstanding chunk. It is idempotent on
// the protocol message id, so WhatsApp's redelivery after a withheld ack
// collapses to a no-op once a write eventually succeeds.
func (r *HistoryRecorder) RecordHistoryNotification(
	ctx context.Context,
	protocolMsgID string,
	notification []byte,
	syncType string,
	chunkOrder int32,
	oldestMsgTS *time.Time,
	disposition string,
) error {
	if _, err := r.repo.RecordNotification(ctx, protocolMsgID, notification, syncType, chunkOrder, oldestMsgTS, disposition); err != nil {
		return fmt.Errorf("record history notification: %w", err)
	}
	return nil
}
