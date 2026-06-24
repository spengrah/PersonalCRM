package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
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

// ReplayAssertion drives a synthetic fact assertion through the REAL
// AssertService write path (validation, proposition identity, events) against an
// existing subject person node. The caller seeds the subject first (e.g. via
// SeedContact, which dual-writes the person node). The assertion's user
// provenance carries a namespace-stable source_id so a re-run corroborates
// (idempotent) rather than duplicating.
//
// Cleanup: the assertion rows ride on the subject node and are removed by the
// harness teardown's assertion step (which deletes assertions on every seeded
// contact node before the person-node delete, since the assertion→node FK is
// RESTRICT).
func (h *Harness) ReplayAssertion(ctx context.Context, subjectNodeID uuid.UUID, spec factory.AssertionSpec) (AssertionResult, error) {
	svc := h.assertService()
	value := spec.ValueText
	req := service.AssertRequest{
		SubjectNodeID: subjectNodeID,
		PredicateKey:  spec.PredicateKey,
		ValueText:     &value,
		Confidence:    spec.Confidence,
		Locators: []service.ProvenanceLocator{{
			SourceKind:   repository.SourceKindUser,
			SourceID:     spec.PropositionKey + ":user",
			ProducerKind: repository.ProducerKindUser,
		}},
	}
	a, err := svc.Assert(ctx, req)
	if err != nil {
		return AssertionResult{}, fmt.Errorf("replay assertion %s: %w", spec.PredicateKey, err)
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
