package replay

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AssertionResult is the settled outcome of an assertion replay.
type AssertionResult struct {
	AssertionID   uuid.UUID
	SubjectNodeID uuid.UUID
	PredicateKey  string
}

// assertService builds an AssertService over the harness's pool + bus + graph
// repos. The graph repos are cheap thin wrappers, so building per-call (mirroring
// the per-source provider construction in the other adapters) keeps the harness
// struct lean. The bus routes the assertion kinds to no consumer, so no River job
// is enqueued and the never-started client is fine.
func (h *Harness) assertService() *service.AssertService {
	return service.NewAssertService(
		h.database.Pool,
		repository.NewNodeRepository(h.database.Queries),
		repository.NewEntityRepository(h.database.Queries),
		repository.NewPredicateRepository(h.database.Queries),
		repository.NewAssertionRepository(h.database.Queries),
		h.bus,
	)
}

// knowledgeCacheUpdater builds the KnowledgeCacheUpdater over the harness's graph
// repos + contact repo — the same wiring harness_setup.go hands ContactService.
// The repos are cheap thin wrappers, so building per-call (mirroring assertService)
// keeps the harness struct lean.
func (h *Harness) knowledgeCacheUpdater() *consumer.KnowledgeCacheUpdater {
	return consumer.NewKnowledgeCacheUpdater(
		repository.NewAssertionRepository(h.database.Queries),
		repository.NewNodeRepository(h.database.Queries),
		h.contactRepo,
	)
}

// ReplayAssertion drives a synthetic fact assertion through the REAL
// AssertService write path (validation, proposition identity, events) against an
// existing subject person node. The caller seeds the subject first (e.g. via
// SeedContact, which dual-writes the person node). The assertion's user
// provenance carries a namespace-stable source_id so a re-run corroborates
// (idempotent) rather than duplicating.
//
// It runs the same one-tx contract as the production write path
// (ContactService.knowledge / EnrichmentService.assertInferredKnowledge): AssertTx
// then, for a cutover predicate, an inline KnowledgeCacheUpdater.RefreshTx in the
// SAME tx, so a seeded lives_in / birthday / how_met assertion leaves its derived
// contact cache column current rather than the production-impossible stale/NULL
// state F8 flagged. A single ReplayAssertion writes one predicate, so it refreshes
// only that predicate's column (narrower than knowledgeWriter.refreshAll, which
// covers a multi-field contact edit). Non-cutover predicates skip the refresh —
// RefreshTx errors on them by design (finding 3), and their columns are not derived.
//
// Cleanup: the assertion rows ride on the subject node and are removed by the
// harness teardown's assertion step (which deletes assertions on every seeded
// contact node before the person-node delete, since the assertion→node FK is
// RESTRICT).
func (h *Harness) ReplayAssertion(ctx context.Context, subjectNodeID uuid.UUID, spec factory.AssertionSpec) (res AssertionResult, err error) {
	req := service.AssertRequest{
		SubjectNodeID: subjectNodeID,
		PredicateKey:  spec.PredicateKey,
		Confidence:    spec.Confidence,
		Locators: []service.ProvenanceLocator{{
			SourceKind:   repository.SourceKindUser,
			SourceID:     spec.PropositionKey + ":user",
			ProducerKind: repository.ProducerKindUser,
		}},
	}
	// Route exactly one value carrier per the spec's kind. scalarCount counts ANY
	// non-nil value pointer, so an edge must set object-only (no empty ValueText)
	// and a bool/date fact must set its scalar only. ObjectNodeID wins (edges carry
	// no scalar), then ValueBool, then ValueDate, else a text fact.
	switch {
	case spec.ObjectNodeID != nil:
		obj := *spec.ObjectNodeID
		req.ObjectNodeID = &obj
	case spec.ValueBool != nil:
		v := *spec.ValueBool
		req.ValueBool = &v
	case spec.ValueDate != nil:
		d := *spec.ValueDate
		req.ValueDate = &d
	default:
		value := spec.ValueText
		req.ValueText = &value
	}

	tx, err := h.database.Pool.Begin(ctx)
	if err != nil {
		return AssertionResult{}, fmt.Errorf("replay assertion %s: begin tx: %w", spec.PredicateKey, err)
	}
	defer func() {
		if rb := tx.Rollback(ctx); rb != nil && !errors.Is(rb, pgx.ErrTxClosed) && err == nil {
			err = rb
		}
	}()

	a, err := h.assertService().AssertTx(ctx, tx, req)
	if err != nil {
		return AssertionResult{}, fmt.Errorf("replay assertion %s: %w", spec.PredicateKey, err)
	}

	// Cutover predicate: refresh the derived cache column inline, in the assert tx,
	// exactly as ContactService.knowledge does. The refresh reads the just-written
	// current-accepted assertion (same tx) and recomputes the column, so an accepted
	// assertion populates the cache and a proposed/pending one leaves it NULL.
	if consumer.IsCacheCutoverPredicate(spec.PredicateKey) {
		if rerr := h.knowledgeCacheUpdater().RefreshTx(ctx, tx, subjectNodeID, spec.PredicateKey); rerr != nil {
			return AssertionResult{}, fmt.Errorf("replay assertion %s: refresh cache: %w", spec.PredicateKey, rerr)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return AssertionResult{}, fmt.Errorf("replay assertion %s: commit: %w", spec.PredicateKey, err)
	}

	// Record the source the assertion events published under so Cleanup captures
	// the root event rows the contact-scoped read misses.
	h.track(func(c *created) { c.addDirectSource("assertion") })
	return AssertionResult{
		AssertionID:   a.ID,
		SubjectNodeID: subjectNodeID,
		PredicateKey:  a.PredicateKey,
	}, nil
}
