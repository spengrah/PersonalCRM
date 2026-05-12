package repository

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// StagingProcessor is the source-neutral seam between the
// InteractionRecorder consumer and per-source staging tables. Concrete
// implementations wrap the per-source repository's
// MarkMessagesProcessedTx, threading the event's source_ref through so
// the SQL predicate can scope the update to rows still claimed for
// that exact session (defends against the stale boundary-shift race).
//
// Returns rows actually updated so the consumer can log a warning
// when zero rows matched (race detected).
type StagingProcessor interface {
	MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error)
}

// StagingProcessorRegistry routes consumer mark-processed calls to the
// correct per-source staging repository. Constructed with one entry
// per source; the consumer calls MarkProcessedTx(ctx, tx, source, ...)
// and the registry dispatches.
//
// Unknown sources are intentionally lenient: the consumer mark-processed
// path is best-effort wrt the interaction insert (the insert is the
// durable contract; mark-processed is a "clear the unprocessed bit"
// follow-up). A logged warning + nil error is correct behavior for an
// unknown source — the operator sees the gap in logs without blocking
// the consumer.
type StagingProcessorRegistry struct {
	processors map[string]StagingProcessor
}

// NewStagingProcessorRegistry builds a registry from a source → processor map.
func NewStagingProcessorRegistry(processors map[string]StagingProcessor) *StagingProcessorRegistry {
	return &StagingProcessorRegistry{processors: processors}
}

// MarkProcessedTx dispatches to the source's processor, threading sessionRef
// through so the underlying SQL can scope the update to rows still
// claimed for that exact session. Returns rows affected (0 for unknown
// sources / empty inputs).
func (r *StagingProcessorRegistry) MarkProcessedTx(
	ctx context.Context, tx pgx.Tx, source string, ids []uuid.UUID, interactionID uuid.UUID, sessionRef string,
) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	p, ok := r.processors[source]
	if !ok {
		log.Warn().
			Str("source", source).
			Int("ids", len(ids)).
			Str("interaction_id", interactionID.String()).
			Msg("staging: no processor registered for source; skipping mark-processed")
		return 0, nil
	}
	return p.MarkProcessedTx(ctx, tx, ids, interactionID, sessionRef)
}
