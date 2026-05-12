package scheduler

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/logger"

	"github.com/riverqueue/river"
)

// PairingTokenJanitor is the narrow interface PairingTokenJanitorWorker
// needs. In production this is *repository.MacHostPairingTokenRepository.
type PairingTokenJanitor interface {
	DeleteExpiredTokens(ctx context.Context) (int64, error)
}

// PairingTokenJanitorWorker periodically removes expired-but-unconsumed
// pairing tokens. Scheduled every 5 minutes via the River periodic-job
// framework (see cmd/crm-api/main.go).
//
// Rejected alternative: rely on `expires_at < NOW()` in ConsumeToken and
// never delete. The table would grow steadily with leaked unconsumed
// tokens; an explicit sweep keeps the working set tiny.
type PairingTokenJanitorWorker struct {
	river.WorkerDefaults[PairingTokenJanitorArgs]
	repo PairingTokenJanitor
}

// NewPairingTokenJanitorWorker constructs the worker over the given
// repository.
func NewPairingTokenJanitorWorker(repo PairingTokenJanitor) *PairingTokenJanitorWorker {
	return &PairingTokenJanitorWorker{repo: repo}
}

// Work implements river.Worker for PairingTokenJanitorArgs.
func (w *PairingTokenJanitorWorker) Work(ctx context.Context, _ *river.Job[PairingTokenJanitorArgs]) error {
	n, err := w.repo.DeleteExpiredTokens(ctx)
	if err != nil {
		return fmt.Errorf("delete expired pairing tokens: %w", err)
	}
	if n > 0 {
		logger.Info().Int64("deleted", n).Msg("pairing token janitor: removed expired tokens")
	}
	return nil
}

// Timeout caps each invocation. The DELETE is bounded by table size
// (typically < 100 rows) so 30s is generous; the limit prevents a
// pathological lock-wait from hanging a worker indefinitely.
func (*PairingTokenJanitorWorker) Timeout(*river.Job[PairingTokenJanitorArgs]) time.Duration {
	return 30 * time.Second
}
