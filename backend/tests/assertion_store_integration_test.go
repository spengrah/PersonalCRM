//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Assertion-store round-trips + constraint coverage against a real DB. Each
// sub-test is namespace-scoped (migrationGenerator) and cleans up its own
// assertion + node closure under t.Parallel(). The assertion → node FK is
// restrict, so cleanup deletes assertions BEFORE nodes (t.Cleanup is LIFO:
// register the assertion delete LAST so it runs FIRST).
//
// The CHECK-constraint sub-tests deliberately drive the INSERT through a raw
// param shape (via a small insert helper) so they can construct rows the typed
// write path would never build — the schema is the last line of defense and
// these prove it.

// assertNodes creates a namespaced person subject + object node pair for an
// assertion test, returning their ids. The caller registers cleanup (assertion
// delete before node delete, since the assertion → node FK is restrict).
func assertNodes(t *testing.T, ctx context.Context, nodeRepo *repository.NodeRepository, prefix string) (subjectID, objectID uuid.UUID) {
	t.Helper()
	subjectID, objectID = uuid.New(), uuid.New()
	_, err := nodeRepo.CreateNode(ctx, subjectID, repository.NodeTypePerson, prefix+"subject")
	require.NoError(t, err)
	_, err = nodeRepo.CreateNode(ctx, objectID, repository.NodeTypePerson, prefix+"object")
	require.NoError(t, err)
	return subjectID, objectID
}

func TestAssertionStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	database, ctx := graphTestDB(t)
	nodeRepo := repository.NewNodeRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	// baseFact builds a minimal valid fact-assertion insert (home_address: text,
	// single) for a subject, with the namespace-unique proposition_key. The caller
	// overrides fields as needed.
	baseFact := func(subjectID uuid.UUID, propKey, value string) repository.InsertAssertionParams {
		text := value
		return repository.InsertAssertionParams{
			SubjectNodeID:  subjectID,
			PredicateKey:   "home_address",
			ValueText:      &text,
			KnowledgeFrom:  accelerated.GetCurrentTime().UTC(),
			Confidence:     80,
			Salience:       45,
			Status:         repository.AssertionStatusAccepted,
			PropositionKey: propKey,
		}
	}

	t.Run("insert assertion + provenance round-trip and reverse lookup", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		spec := gen.FactAssertion("home_address")
		inserted, err := assertionRepo.InsertAssertion(ctx, baseFact(subjectID, spec.PropositionKey, spec.ValueText))
		require.NoError(t, err)
		assert.NotEqual(t, uuid.Nil, inserted.ID)
		require.NotNil(t, inserted.ValueText)
		assert.Equal(t, spec.ValueText, *inserted.ValueText)
		assert.Equal(t, repository.AssertionStatusAccepted, inserted.Status)
		assert.Nil(t, inserted.KnowledgeTo)

		got, err := assertionRepo.GetAssertion(ctx, inserted.ID)
		require.NoError(t, err)
		assert.Equal(t, inserted.ID, got.ID)
		assert.Equal(t, subjectID, got.SubjectNodeID)

		// Append a provenance locator.
		field := "body"
		quote := gen.Prefix() + "they said so"
		sourceID := gen.Prefix() + "msg-1"
		ins, err := assertionRepo.InsertProvenance(ctx, repository.InsertProvenanceParams{
			AssertionID:  inserted.ID,
			LocatorHash:  gen.Prefix() + "loc-1",
			SourceKind:   repository.SourceKindCommsMessage,
			SourceID:     sourceID,
			ProducerKind: repository.ProducerKindExtractor,
			Field:        &field,
			Quote:        &quote,
		})
		require.NoError(t, err)
		assert.True(t, ins, "first locator insert reports a row inserted")

		// Forward lookup (by assertion_id).
		provs, err := assertionRepo.ListProvenance(ctx, inserted.ID)
		require.NoError(t, err)
		require.Len(t, provs, 1)
		assert.Equal(t, repository.SourceKindCommsMessage, provs[0].SourceKind)
		require.NotNil(t, provs[0].Quote)
		assert.Equal(t, quote, *provs[0].Quote)
		assert.False(t, provs[0].CreatedAt.IsZero(), "created_at is preserved at the repository boundary")

		// Reverse lookup (by source_kind, source_id) — the (source_kind, source_id)
		// index answers "what did this source produce".
		bySource, err := assertionRepo.ListProvenanceBySource(ctx, repository.SourceKindCommsMessage, sourceID)
		require.NoError(t, err)
		require.Len(t, bySource, 1)
		assert.Equal(t, inserted.ID, bySource[0].AssertionID, "reverse lookup resolves back to the assertion")
		assert.Equal(t, sourceID, bySource[0].SourceID)

		// Namespace-scoped count read.
		n, err := support.CountAssertionsForSubject(ctx, subjectID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)
	})

	t.Run("provenance (assertion_id, locator_hash) conflict is a no-op; a different hash inserts", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		spec := gen.FactAssertion("home_address")
		a, err := assertionRepo.InsertAssertion(ctx, baseFact(subjectID, spec.PropositionKey, spec.ValueText))
		require.NoError(t, err)

		locA := repository.InsertProvenanceParams{
			AssertionID:  a.ID,
			LocatorHash:  gen.Prefix() + "loc-A",
			SourceKind:   repository.SourceKindUser,
			SourceID:     gen.Prefix() + "edit-1",
			ProducerKind: repository.ProducerKindUser,
		}
		ins, err := assertionRepo.InsertProvenance(ctx, locA)
		require.NoError(t, err)
		assert.True(t, ins, "first insert reports inserted")

		// SAME (assertion_id, locator_hash) → ON CONFLICT DO NOTHING no-op.
		ins, err = assertionRepo.InsertProvenance(ctx, locA)
		require.NoError(t, err)
		assert.False(t, ins, "duplicate locator reports NOT inserted")

		// A DIFFERENT locator_hash on the same assertion (a different span/version)
		// inserts a new corroborating row.
		locB := locA
		locB.LocatorHash = gen.Prefix() + "loc-B"
		ins, err = assertionRepo.InsertProvenance(ctx, locB)
		require.NoError(t, err)
		assert.True(t, ins, "a distinct locator inserts")

		provs, err := assertionRepo.ListProvenance(ctx, a.ID)
		require.NoError(t, err)
		assert.Len(t, provs, 2, "exactly two distinct locators")
	})

	t.Run("FindLiveProposition returns the live row and skips terminal rows", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		// A terminal (superseded) row with the SAME proposition_key must NOT shadow
		// the live one — the live-proposition index is partial on the live predicate.
		spec := gen.FactAssertion("home_address")
		terminal := baseFact(subjectID, spec.PropositionKey, spec.ValueText)
		now := accelerated.GetCurrentTime().UTC()
		terminal.Status = repository.AssertionStatusSuperseded
		closure := repository.ClosureReasonSuperseded
		terminal.ClosureReason = &closure
		terminal.KnowledgeTo = &now
		_, err := assertionRepo.InsertAssertion(ctx, terminal)
		require.NoError(t, err)

		// No live row yet → ErrNotFound.
		_, err = assertionRepo.FindLiveProposition(ctx, spec.PropositionKey)
		require.ErrorIs(t, err, db.ErrNotFound)

		// Now a live row at the same key.
		live, err := assertionRepo.InsertAssertion(ctx, baseFact(subjectID, spec.PropositionKey, spec.ValueText))
		require.NoError(t, err)

		found, err := assertionRepo.FindLiveProposition(ctx, spec.PropositionKey)
		require.NoError(t, err)
		assert.Equal(t, live.ID, found.ID, "FindLiveProposition returns the live row, not the terminal one")
	})

	t.Run("live-proposition partial unique rejects a 2nd live row of the same key", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		spec := gen.FactAssertion("home_address")
		_, err := assertionRepo.InsertAssertion(ctx, baseFact(subjectID, spec.PropositionKey, spec.ValueText))
		require.NoError(t, err)

		// A second LIVE (accepted, knowledge-open) row at the same proposition_key
		// violates idx_assertion_live_proposition.
		_, err = assertionRepo.InsertAssertion(ctx, baseFact(subjectID, spec.PropositionKey, spec.ValueText))
		require.Error(t, err)
		assert.True(t, isUniqueViolation(err), "expected a 23505 unique violation, got %v", err)
	})

	t.Run("GetCurrentAccepted valid-time logic (open/closed/future ranges)", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		now := accelerated.GetCurrentTime().UTC()

		// Open-ended accepted row → current.
		open := gen.FactAssertion("home_address")
		_, err := assertionRepo.InsertAssertion(ctx, baseFact(subjectID, open.PropositionKey, open.ValueText))
		require.NoError(t, err)
		cur, err := assertionRepo.GetCurrentAccepted(ctx, subjectID, "home_address", now)
		require.NoError(t, err)
		require.NotNil(t, cur.ValueText)
		assert.Equal(t, open.ValueText, *cur.ValueText)

		// A future-start row is NOT current yet (valid_from > now).
		futStart := now.Add(72 * time.Hour)
		fut := baseFact(subjectID, gen.Prefix()+"future-prop", gen.Prefix()+"future-val")
		fut.PredicateKey = "job_title"
		fut.ValidFrom = &futStart
		_, err = assertionRepo.InsertAssertion(ctx, fut)
		require.NoError(t, err)
		_, err = assertionRepo.GetCurrentAccepted(ctx, subjectID, "job_title", now)
		require.ErrorIs(t, err, db.ErrNotFound, "a future-start row is not yet current")
		// But it IS current once now passes its valid_from.
		cur, err = assertionRepo.GetCurrentAccepted(ctx, subjectID, "job_title", futStart.Add(time.Hour))
		require.NoError(t, err)
		assert.Equal(t, "job_title", cur.PredicateKey)

		// A past-bounded row (valid_to <= now) is NOT current.
		pastFrom := now.Add(-72 * time.Hour)
		pastTo := now.Add(-24 * time.Hour)
		past := baseFact(subjectID, gen.Prefix()+"past-prop", gen.Prefix()+"past-val")
		past.PredicateKey = "preference"
		past.ValidFrom = &pastFrom
		past.ValidTo = &pastTo
		_, err = assertionRepo.InsertAssertion(ctx, past)
		require.NoError(t, err)
		_, err = assertionRepo.GetCurrentAccepted(ctx, subjectID, "preference", now)
		require.ErrorIs(t, err, db.ErrNotFound, "a past-bounded row is not current")
	})

	t.Run("exactly-one-payload CHECK rejects zero and two payloads", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, objectID := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		// Zero payloads.
		zero := baseFact(subjectID, gen.Prefix()+"zero", "")
		zero.ValueText = nil
		_, err := assertionRepo.InsertAssertion(ctx, zero)
		require.Error(t, err, "zero payloads must violate assertion_one_payload")
		assert.True(t, isCheckViolation(err), "expected a CHECK violation, got %v", err)

		// Two payloads (an object AND a text value).
		two := baseFact(subjectID, gen.Prefix()+"two", gen.Prefix()+"val")
		two.PredicateKey = "partner_of"
		two.ObjectNodeID = &objectID
		_, err = assertionRepo.InsertAssertion(ctx, two)
		require.Error(t, err, "two payloads must violate assertion_one_payload")
		assert.True(t, isCheckViolation(err), "expected a CHECK violation, got %v", err)
	})

	t.Run("confidence / salience BETWEEN 0 AND 100 CHECKs reject out-of-range values", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID := uuid.New()
		_, err := nodeRepo.CreateNode(ctx, subjectID, repository.NodeTypePerson, gen.Prefix()+"subject")
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		// confidence above the [0,100] range.
		hiConf := baseFact(subjectID, gen.Prefix()+"hi-conf", gen.Prefix()+"v")
		hiConf.Confidence = 101
		_, err = assertionRepo.InsertAssertion(ctx, hiConf)
		require.Error(t, err, "confidence=101 must violate the confidence range CHECK")
		assert.True(t, isCheckViolation(err), "expected a CHECK violation, got %v", err)

		// salience below the [0,100] range.
		loSal := baseFact(subjectID, gen.Prefix()+"lo-sal", gen.Prefix()+"v")
		loSal.Salience = -1
		_, err = assertionRepo.InsertAssertion(ctx, loSal)
		require.Error(t, err, "salience=-1 must violate the salience range CHECK")
		assert.True(t, isCheckViolation(err), "expected a CHECK violation, got %v", err)
	})

	t.Run("terminal-knowledge_to CHECK rejects both inconsistent shapes", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		now := accelerated.GetCurrentTime().UTC()

		// Accepted WITH knowledge_to → violation (a live status must not be closed).
		acceptedClosed := baseFact(subjectID, gen.Prefix()+"acc-closed", gen.Prefix()+"v1")
		acceptedClosed.KnowledgeTo = &now
		_, err := assertionRepo.InsertAssertion(ctx, acceptedClosed)
		require.Error(t, err)
		assert.True(t, isCheckViolation(err), "accepted+knowledge_to must violate the terminal CHECK, got %v", err)

		// Superseded WITHOUT knowledge_to → violation (a terminal status must close).
		terminalOpen := baseFact(subjectID, gen.Prefix()+"term-open", gen.Prefix()+"v2")
		terminalOpen.Status = repository.AssertionStatusSuperseded
		closure := repository.ClosureReasonSuperseded
		terminalOpen.ClosureReason = &closure
		// KnowledgeTo left nil.
		_, err = assertionRepo.InsertAssertion(ctx, terminalOpen)
		require.Error(t, err)
		assert.True(t, isCheckViolation(err), "superseded-without-knowledge_to must violate the terminal CHECK, got %v", err)
	})

	t.Run("valid_range / knowledge_range CHECKs reject inverted ranges", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		now := accelerated.GetCurrentTime().UTC()

		// valid_to <= valid_from → assertion_valid_range violation.
		from := now
		to := now.Add(-time.Hour)
		invalidValid := baseFact(subjectID, gen.Prefix()+"vr", gen.Prefix()+"v")
		invalidValid.ValidFrom = &from
		invalidValid.ValidTo = &to
		_, err := assertionRepo.InsertAssertion(ctx, invalidValid)
		require.Error(t, err)
		assert.True(t, isCheckViolation(err), "inverted valid range must violate, got %v", err)

		// knowledge_to < knowledge_from → assertion_knowledge_range violation (use a
		// terminal status so knowledge_to is allowed to be set at all).
		kFrom := now
		kTo := now.Add(-time.Hour)
		invalidKnow := baseFact(subjectID, gen.Prefix()+"kr", gen.Prefix()+"v")
		invalidKnow.KnowledgeFrom = kFrom
		invalidKnow.KnowledgeTo = &kTo
		invalidKnow.Status = repository.AssertionStatusSuperseded
		closure := repository.ClosureReasonSuperseded
		invalidKnow.ClosureReason = &closure
		_, err = assertionRepo.InsertAssertion(ctx, invalidKnow)
		require.Error(t, err)
		assert.True(t, isCheckViolation(err), "inverted knowledge range must violate, got %v", err)
	})

	t.Run("value_num finiteness CHECK rejects NaN and Inf", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		nan := math.NaN()
		posInf := math.Inf(1)
		negInf := math.Inf(-1)
		for name, v := range map[string]float64{"NaN": nan, "+Inf": posInf, "-Inf": negInf} {
			bad := repository.InsertAssertionParams{
				SubjectNodeID:  subjectID,
				PredicateKey:   "home_address",
				ValueNum:       &v,
				KnowledgeFrom:  accelerated.GetCurrentTime().UTC(),
				Confidence:     80,
				Salience:       45,
				Status:         repository.AssertionStatusAccepted,
				PropositionKey: gen.Prefix() + "num-" + name,
			}
			_, err := assertionRepo.InsertAssertion(ctx, bad)
			require.Errorf(t, err, "%s value_num must be rejected", name)
			assert.Truef(t, isCheckViolation(err), "%s must violate assertion_value_num_finite, got %v", name, err)
		}

		// A real finite value_num is accepted.
		fin := 42.5
		good := repository.InsertAssertionParams{
			SubjectNodeID:  subjectID,
			PredicateKey:   "home_address",
			ValueNum:       &fin,
			KnowledgeFrom:  accelerated.GetCurrentTime().UTC(),
			Confidence:     80,
			Salience:       45,
			Status:         repository.AssertionStatusAccepted,
			PropositionKey: gen.Prefix() + "num-finite",
		}
		_, err := assertionRepo.InsertAssertion(ctx, good)
		require.NoError(t, err, "a finite value_num is accepted")
	})

	t.Run("DEFERRABLE self-FK permits insert-new-then-close-prior within one tx", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		// Seed the prior (accepted, live).
		prior, err := assertionRepo.InsertAssertion(ctx, baseFact(subjectID, gen.Prefix()+"defer-prior", gen.Prefix()+"NYC"))
		require.NoError(t, err)

		// In ONE tx: insert the NEW row (its proposition_key differs so it does not
		// collide on the live index), then close the prior pointing superseded_by at
		// the new row. With the DEFERRABLE self-FK the order is valid regardless.
		now := accelerated.GetCurrentTime().UTC()
		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()

		successor, err := assertionRepo.InsertAssertionTx(ctx, tx, baseFact(subjectID, gen.Prefix()+"defer-successor", gen.Prefix()+"LA"))
		require.NoError(t, err)
		closure := repository.ClosureReasonSuperseded
		require.NoError(t, assertionRepo.CloseAssertionTx(ctx, tx, repository.CloseAssertionParams{
			ID:            prior.ID,
			ValidTo:       &now,
			Status:        repository.AssertionStatusSuperseded,
			ClosureReason: &closure,
			SupersededBy:  &successor.ID,
			KnowledgeTo:   &now,
		}))
		require.NoError(t, tx.Commit(ctx))

		closed, err := assertionRepo.GetAssertion(ctx, prior.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.AssertionStatusSuperseded, closed.Status)
		require.NotNil(t, closed.SupersededBy)
		assert.Equal(t, successor.ID, *closed.SupersededBy)
	})

	t.Run("concurrent duplicate inserts leave exactly one live row", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		subjectID, _ := assertNodes(t, ctx, nodeRepo, gen.Prefix())
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteAssertionsForNode(ctx, subjectID) })

		propKey := gen.Prefix() + "concurrent-prop"
		params := baseFact(subjectID, propKey, gen.Prefix()+"v")

		// Two goroutines race to insert the SAME proposition_key; the DB constraint
		// (idx_assertion_live_proposition) admits exactly one. (The write-path
		// savepoint-recover lives in the assert() service layer; here we prove the
		// DB invariant the recover relies on.)
		const n = 2
		var wg sync.WaitGroup
		results := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				_, results[idx] = assertionRepo.InsertAssertion(ctx, params)
			}(i)
		}
		wg.Wait()

		successes, uniqueViolations := 0, 0
		for _, err := range results {
			switch {
			case err == nil:
				successes++
			case isUniqueViolation(err):
				uniqueViolations++
			default:
				t.Fatalf("unexpected error from concurrent insert: %v", err)
			}
		}
		assert.Equal(t, 1, successes, "exactly one insert wins")
		assert.Equal(t, n-1, uniqueViolations, "the rest collide on the live-proposition unique")

		// Exactly one live row remains.
		_, err := assertionRepo.FindLiveProposition(ctx, propKey)
		require.NoError(t, err)
	})
}

// isUniqueViolation reports whether err is a Postgres 23505 unique_violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isCheckViolation reports whether err is a Postgres 23514 check_violation.
func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
