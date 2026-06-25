package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// knowledgeCacheRefresher is the inline tx surface ContactService /
// EnrichmentService use to refresh a contact's derived cache column the moment
// the assertion lands (so a user edit's location/birthday/how_met is visible on
// commit, not after the async river worker fires). Satisfied by
// *consumer.KnowledgeCacheUpdater. Defined here so the service layer doesn't
// import the consumer package's full graph.
type knowledgeCacheRefresher interface {
	RefreshTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, predicateKey string) error
}

// knowledgeFieldProvenance carries the per-source provenance the authority flip
// stamps on an emitted knowledge assertion. A direct user edit uses
// SourceKindUser + ProducerKindUser; the enrichment path uses SourceKindAgentSession
// + ProducerKindAgent (inferred from external contact data). SourceIDPrefix +
// the field name + contact id form the deterministic locator source_id so a
// re-run / retry collides on the provenance unique and no-ops.
type knowledgeFieldProvenance struct {
	SourceKind   string
	ProducerKind string
	// SourceIDPrefix is the stable namespace for the locator source_id
	// ("edit", "merge", "enrichment", "contact_col"). The full source_id is
	// "<prefix>:<contactID>:<field>".
	SourceIDPrefix string
	// KnowledgeFromOverride preserves historical knowledge-time for a backfill
	// (the contact row's created_at). Live edits leave it nil → knowledge_from
	// defaults to now.
	KnowledgeFromOverride *time.Time
}

// knowledgeFieldValues is the trio of profile values the cutover routes to the
// assertion store. A nil value means "no value for this field" (the caller has
// already resolved the effective value); the update path closes a slot whose
// value transitions non-nil→nil.
type knowledgeFieldValues struct {
	Location *string
	Birthday *time.Time
	HowMet   *string
}

// normalized collapses a whitespace-only Location/HowMet to nil so an
// empty-string field (the HTTP layer forwards request strings verbatim) reads as
// "no value" — a create skips it and an update treats it as a CLEAR, matching the
// pre-cutover leniency where a blank field was unset rather than rejected.
func (v knowledgeFieldValues) normalized() knowledgeFieldValues {
	v.Location = nonBlank(v.Location)
	v.HowMet = nonBlank(v.HowMet)
	return v
}

// nonBlank returns nil when s is nil or whitespace-only, else s unchanged.
func nonBlank(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	return s
}

// knowledgeWriter emits knowledge assertions for the cutover predicates and
// refreshes the cache inline. It wraps AssertService (the validated write path)
// + the cache refresher (sole-writer column update). ContactService and
// EnrichmentService both construct one against their tx-bound repos.
type knowledgeWriter struct {
	assertSvc *AssertService
	cache     knowledgeCacheRefresher
}

// newKnowledgeWriter constructs the writer. Both dependencies are required; a
// nil assertSvc or cache is a wiring bug (the authority flip cannot persist
// location/birthday/how_met without them).
func newKnowledgeWriter(assertSvc *AssertService, cache knowledgeCacheRefresher) *knowledgeWriter {
	return &knowledgeWriter{assertSvc: assertSvc, cache: cache}
}

// assertCreate emits accepted assertions for each non-nil field value on a
// freshly-created contact, then refreshes that contact's cache columns inline.
// A nil value is skipped (no closure needed — a new contact has no prior slot).
func (w *knowledgeWriter) assertCreate(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, values knowledgeFieldValues, prov knowledgeFieldProvenance) error {
	values = values.normalized()
	if values.Location != nil {
		if err := w.assertLocation(ctx, tx, contactID, *values.Location, prov); err != nil {
			return err
		}
	}
	if values.Birthday != nil {
		if err := w.assertBirthday(ctx, tx, contactID, *values.Birthday, prov); err != nil {
			return err
		}
	}
	if values.HowMet != nil {
		if err := w.assertHowMet(ctx, tx, contactID, *values.HowMet, prov); err != nil {
			return err
		}
	}
	return w.refreshAll(ctx, tx, contactID)
}

// applyUpdate reconciles each field against the contact's existing (cache)
// value, then refreshes the cache inline. `existing` is the contact's pre-update
// state (cache columns reflect the current-accepted assertions).
//
// location/birthday and how_met intentionally differ on a nil incoming value,
// because the contact form manages location/birthday but NOT how_met:
//   - location/birthday: nil means the user cleared the field → close the slot.
//   - how_met: nil means the form simply doesn't carry how_met (it has no
//     how_met input), NOT a user clear → NO-OP, leaving the existing how_met
//     assertion untouched. (This both avoids polluting the assertion history with
//     a spurious closure on every edit AND preserves how_met across edits — the
//     pre-cutover SQL path wiped how_met on every save, which this fixes.)
//
// An explicit how_met value (non-nil after trim) is always asserted.
func (w *knowledgeWriter) applyUpdate(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, next, existing knowledgeFieldValues, prov knowledgeFieldProvenance) error {
	next = next.normalized()
	existing = existing.normalized()
	if err := w.reconcileClearable(ctx, tx, contactID, next.Location, existing.Location, repository.PredicateLivesIn, func(label string) error {
		return w.assertLocation(ctx, tx, contactID, label, prov)
	}); err != nil {
		return err
	}
	if err := w.reconcileBirthday(ctx, tx, contactID, next.Birthday, existing.Birthday, prov); err != nil {
		return err
	}
	// how_met: assert when present, NO-OP when absent (never close) — see the
	// method doc above.
	if next.HowMet != nil {
		if err := w.assertHowMet(ctx, tx, contactID, *next.HowMet, prov); err != nil {
			return err
		}
	}
	return w.refreshAll(ctx, tx, contactID)
}

// reconcileClearable handles a form-managed string field where a nil incoming
// value is a legitimate user clear: assert a non-nil value, close the slot when
// the value transitions non-nil→nil.
func (w *knowledgeWriter) reconcileClearable(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, next, existing *string, predicateKey string, assert func(string) error) error {
	switch {
	case next != nil:
		return assert(*next)
	case existing != nil:
		return w.closeSlot(ctx, tx, contactID, predicateKey)
	default:
		return nil
	}
}

func (w *knowledgeWriter) reconcileBirthday(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, next, existing *time.Time, prov knowledgeFieldProvenance) error {
	switch {
	case next != nil:
		return w.assertBirthday(ctx, tx, contactID, *next, prov)
	case existing != nil:
		return w.closeSlot(ctx, tx, contactID, repository.PredicateBirthday)
	default:
		return nil
	}
}

// assertLocation ensures the place entity node for the label, then asserts the
// lives_in edge. A whitespace-only label is skipped (no place node minted for a
// blank string — the cache column simply stays empty, matching the pre-cutover
// leniency where a blank location was stored verbatim rather than rejected).
func (w *knowledgeWriter) assertLocation(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, label string, prov knowledgeFieldProvenance) error {
	if strings.TrimSpace(label) == "" {
		return nil
	}
	placeID, err := w.assertSvc.EnsurePlaceTx(ctx, tx, label)
	if err != nil {
		return fmt.Errorf("ensure place node: %w", err)
	}
	req := AssertRequest{
		SubjectNodeID:         contactID,
		PredicateKey:          repository.PredicateLivesIn,
		ObjectNodeID:          &placeID,
		Confidence:            knowledgeAssertConfidence(prov),
		KnowledgeFromOverride: prov.KnowledgeFromOverride,
		Locators:              []ProvenanceLocator{w.locator(prov, contactID, "location")},
	}
	if _, err := w.assertSvc.AssertTx(ctx, tx, req); err != nil {
		return fmt.Errorf("assert lives_in: %w", err)
	}
	return nil
}

func (w *knowledgeWriter) assertBirthday(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, birthday time.Time, prov knowledgeFieldProvenance) error {
	value := birthday
	req := AssertRequest{
		SubjectNodeID:         contactID,
		PredicateKey:          repository.PredicateBirthday,
		ValueDate:             &value,
		Confidence:            knowledgeAssertConfidence(prov),
		KnowledgeFromOverride: prov.KnowledgeFromOverride,
		Locators:              []ProvenanceLocator{w.locator(prov, contactID, "birthday")},
	}
	if _, err := w.assertSvc.AssertTx(ctx, tx, req); err != nil {
		return fmt.Errorf("assert birthday: %w", err)
	}
	return nil
}

func (w *knowledgeWriter) assertHowMet(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, howMet string, prov knowledgeFieldProvenance) error {
	value := howMet
	req := AssertRequest{
		SubjectNodeID:         contactID,
		PredicateKey:          repository.PredicateHowMet,
		ValueText:             &value,
		Confidence:            knowledgeAssertConfidence(prov),
		KnowledgeFromOverride: prov.KnowledgeFromOverride,
		Locators:              []ProvenanceLocator{w.locator(prov, contactID, "how_met")},
	}
	if _, err := w.assertSvc.AssertTx(ctx, tx, req); err != nil {
		return fmt.Errorf("assert how_met: %w", err)
	}
	return nil
}

// closeSlot closes the current-accepted assertion for a single-cardinality slot
// (the user cleared the field). AssertClosureTx is a no-op when there is no
// current value, so a redundant clear is safe.
func (w *knowledgeWriter) closeSlot(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, predicateKey string) error {
	if err := w.assertSvc.AssertClosureTx(ctx, tx, ClosureRequest{
		SubjectNodeID: contactID,
		PredicateKey:  predicateKey,
	}); err != nil {
		return fmt.Errorf("close %s: %w", predicateKey, err)
	}
	return nil
}

// refreshAll recomputes all three cache columns inline so the contact row the
// caller returns reflects the just-written assertions on commit.
func (w *knowledgeWriter) refreshAll(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) error {
	for _, predicateKey := range []string{repository.PredicateLivesIn, repository.PredicateBirthday, repository.PredicateHowMet} {
		if err := w.cache.RefreshTx(ctx, tx, contactID, predicateKey); err != nil {
			return fmt.Errorf("refresh %s cache: %w", predicateKey, err)
		}
	}
	return nil
}

// locator builds the deterministic provenance locator for a field write. The
// source_id is "<prefix>:<contactID>:<field>" so a re-run (backfill) or retry
// hashes to the same locator and the provenance ON CONFLICT no-ops.
func (w *knowledgeWriter) locator(prov knowledgeFieldProvenance, contactID uuid.UUID, field string) ProvenanceLocator {
	return ProvenanceLocator{
		SourceKind:   prov.SourceKind,
		SourceID:     fmt.Sprintf("%s:%s:%s", prov.SourceIDPrefix, contactID, field),
		ProducerKind: prov.ProducerKind,
	}
}

// knowledgeAssertConfidence is the confidence stamped on an emitted knowledge
// assertion. A direct user edit / migration is full confidence (the user
// asserted it); an inferred enrichment is high but not certain.
func knowledgeAssertConfidence(prov knowledgeFieldProvenance) int16 {
	if prov.ProducerKind == repository.ProducerKindUser {
		return 100
	}
	return 80
}
