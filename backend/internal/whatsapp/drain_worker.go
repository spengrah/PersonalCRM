package whatsapp

import (
	"context"

	"personal-crm/backend/internal/consumer/consumerjobs"

	"github.com/riverqueue/river"
)

// HistoryDrainWorker is the River adapter over HistoryDrainer. It holds no
// logic of its own: the protocol, its failure taxonomy and its fencing all live
// in the drainer, which is unit-testable without River.
type HistoryDrainWorker struct {
	river.WorkerDefaults[consumerjobs.WhatsAppHistoryDrainArgs]
	drainer *HistoryDrainer
}

// NewHistoryDrainWorker wraps a drainer for River.
func NewHistoryDrainWorker(drainer *HistoryDrainer) *HistoryDrainWorker {
	return &HistoryDrainWorker{drainer: drainer}
}

// Work drains the whole claimable backlog. A returned error lands in river_job,
// which is deliberate: a silently-swallowed retry would make a stuck backfill
// invisible.
func (w *HistoryDrainWorker) Work(ctx context.Context, _ *river.Job[consumerjobs.WhatsAppHistoryDrainArgs]) error {
	return w.drainer.Drain(ctx)
}
