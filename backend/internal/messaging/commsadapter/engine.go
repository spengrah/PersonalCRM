package commsadapter

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// NewInteractionFinder wraps *repository.InteractionRepository and exposes the
// source-neutral aggregation.InteractionFinder surface. Lives here (not in the
// repository package) so the repository type stays free of aggregation-package
// imports.
func NewInteractionFinder(repo *repository.InteractionRepository) aggregation.InteractionFinder {
	return &interactionFinderAdapter{repo: repo}
}

// EventLookup adapts *events.Bus to aggregation.EventLookup, returning the
// UNTYPED-nil interface value when bus is nil.
//
// Returning nil here rather than at each call site is the point: a typed-nil
// concrete pointer satisfies the engine's nil guard as non-nil and silently
// bypasses it (see the EventPublisher note in
// messaging/aggregation/interfaces.go).
func EventLookup(bus *events.Bus) aggregation.EventLookup {
	if bus == nil {
		return nil
	}
	return &busEventLookup{bus: bus}
}

// Publisher adapts *events.Bus to aggregation.EventPublisher with the same
// untyped-nil contract as EventLookup.
func Publisher(bus *events.Bus) aggregation.EventPublisher {
	if bus == nil {
		return nil
	}
	return bus
}

// NewEngine constructs the shared aggregation engine for a comms_message-backed
// source. Callers supply their own SourceAdapter (normally NewAdapter) and their
// own burst/reply windows; everything else is uniform across comms sources.
//
// A nil eventBus, pool, or enqueuer is safe: eventBus is converted to the
// untyped-nil interface by Publisher/EventLookup, and pool/enqueuer are already
// interface-typed parameters the engine nil-guards itself.
func NewEngine(
	adapter aggregation.SourceAdapter,
	burstWindowHours, replyBridgeHours int,
	commsRepo *repository.CommsMessageRepository,
	interactionRepo *repository.InteractionRepository,
	promoter aggregation.InteractionPromoter,
	extender aggregation.InteractionExtender,
	eventBus *events.Bus,
	pool aggregation.TxBeginner,
	enqueuer aggregation.ConsumerJobEnqueuer,
) *aggregation.Engine {
	return aggregation.NewEngine(
		adapter,
		NewStore(commsRepo, adapter.SourceName()),
		NewInteractionFinder(interactionRepo),
		promoter,
		extender,
		Publisher(eventBus),
		burstWindowHours,
		replyBridgeHours,
		pool,
		EventLookup(eventBus),
		enqueuer,
	)
}

// busEventLookup adapts *events.Bus to aggregation.EventLookup:
// (uuid.Nil, false, nil) on db.ErrNotFound; (id, true, nil) on hit; non-nil
// error on infrastructure failure. Constructed only by EventLookup.
type busEventLookup struct{ bus *events.Bus }

func (l *busEventLookup) FindEventBySourceRef(ctx context.Context, source, sourceID string) (uuid.UUID, bool, error) {
	env, err := l.bus.FindEventBySource(ctx, source, sourceID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	return env.ID, true, nil
}

// interactionFinderAdapter wraps *repository.InteractionRepository and exposes
// the source-neutral aggregation.InteractionFinder surface. Constructed only by
// NewInteractionFinder.
type interactionFinderAdapter struct {
	repo *repository.InteractionRepository
}

func (a *interactionFinderAdapter) FindRecentBySourceAndDirection(
	ctx context.Context,
	contactID uuid.UUID,
	source, direction, sourceRefPrefix string,
	windowStart, windowEnd time.Time,
) (*repository.Interaction, error) {
	return a.repo.FindRecentInteractionBySourceAndDirection(ctx, contactID, source, direction, sourceRefPrefix, windowStart, windowEnd)
}

func (a *interactionFinderAdapter) FindRecentOutboundBySource(
	ctx context.Context,
	contactID uuid.UUID,
	source, sourceRefPrefix string,
	windowStart, windowEnd time.Time,
) (*repository.Interaction, error) {
	return a.repo.FindRecentOutboundInteractionBySource(ctx, contactID, source, sourceRefPrefix, windowStart, windowEnd)
}

func (a *interactionFinderAdapter) GetInteraction(ctx context.Context, id uuid.UUID) (*repository.Interaction, error) {
	return a.repo.GetInteraction(ctx, id)
}
