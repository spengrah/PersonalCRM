// Package synthetic is the top-level helper API for the synthetic-seed toolkit.
// It re-exports the ergonomic surface (factory generator + replay harness) so a
// test in backend/tests/ can seed contacts, replay synthetic source input
// through the REAL ingestion pipeline, and assert the settled graph — without
// reaching into the factory/replay sub-packages directly.
//
//	ctx := context.Background()                  // the test's long-lived base ctx
//	h := synthetic.NewHarness(t, ctx, database)  // ctx is MANDATORY (client.Start uses it)
//	gen := h.Generator()                         // seeded, namespaced, live anchor
//	alice, _ := h.SeedContact(ctx, gen.Contact(factory.WithEmail()))
//	res, _ := h.ReplayGmail(ctx, alice.ID, gen.GmailMessage(spec, factory.MatchSeeded))
//	// graph is settled here; assert via h.InteractionRepo() etc.
//
// SeedParams + SeedAll provide the mode-(b) "fast full-seed" shape (the function
// surface + a minimal default dataset). The populated prod-shaped/dev profiles
// and entrypoint wiring are later elements; this is the forward-compatible seam.
package synthetic

import (
	"context"
	"fmt"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
)

// Harness is the replay harness re-exported at the package root.
type Harness = replay.Harness

// NewHarness builds a replay harness for a test. ctx is MANDATORY and is the
// exact context passed to the River client's Start (NOT a timeout-derived ctx).
func NewHarness(t *testing.T, ctx context.Context, database *db.Database) *Harness {
	return replay.NewHarness(t, ctx, database)
}

// NewHarnessForNamespace builds a harness with an explicit namespace + seed so
// each sub-test gets a unique namespace (shared-test-DB isolation).
func NewHarnessForNamespace(t *testing.T, ctx context.Context, database *db.Database, namespace string, seed uint64) *Harness {
	return replay.NewHarnessForNamespace(t, ctx, database, namespace, seed)
}

// NewHarnessWithDB builds a harness without a *testing.T (non-test callers —
// future entrypoints/staging). Returns the harness + a quiesce/cleanup closure +
// an error (building/starting the client, wiring repos, and seeding the
// synthetic host can all fail).
func NewHarnessWithDB(ctx context.Context, database *db.Database) (*Harness, func(context.Context) error, error) {
	return replay.NewHarnessWithDB(ctx, database)
}

// Counts tunes the per-entity volume SeedAll produces. The forward-compatible
// volume seam; DefaultParams supplies a small "minimal-scoped" shape.
type Counts struct {
	SeededContacts int // contacts seeded with a matching identifier per source
}

// SeedParams is the profile/volume seam consumed by SeedAll. Namespace + Seed
// make the run deterministic and isolation-scoped. Distributions/profile tuning
// land with later elements; this carries the minimal viable surface.
type SeedParams struct {
	Namespace string
	Seed      uint64
	Counts    Counts
}

// DefaultParams is the small, namespaced "minimal-scoped" shape so the core is
// usable and testable out of the box.
func DefaultParams() SeedParams {
	return SeedParams{
		Namespace: "seedall",
		Seed:      factory.DefaultSeed,
		Counts:    Counts{SeededContacts: 1},
	}
}

// SeedAllResult reports what SeedAll produced (for assertions).
//
// GmailIdempotencyProbe carries the first Gmail (contactID, message spec) the
// run replayed so a smoke test can re-replay the SAME source payload and assert
// idempotency (no duplicate comms_message row), since SeedAll's contact creation
// is not itself upsert-idempotent (re-running SeedAll seeds a fresh dataset).
type SeedAllResult struct {
	GmailContactIDs    []string
	TelegramContactIDs []string

	GmailIdempotencyProbe *GmailReplayProbe
}

// GmailReplayProbe is a captured (contactID, spec) pair the caller can re-feed
// to Harness.ReplayGmail to prove source-message idempotency.
type GmailReplayProbe struct {
	ContactID uuid.UUID
	Spec      factory.GmailMessageSpec
}

// SeedAll builds a representative full dataset in one call (the mode-(b) fast
// full-seed shape). It seeds contacts and replays a settled message per source
// for each.
//
// Source-message idempotency rides on the per-adapter replay: replaying the same
// synthetic source payload twice upserts/dedups to the same row (stable
// source-ids; the event bus dedups on (source, source_id)). Contact creation is
// NOT upsert-idempotent — calling SeedAll twice seeds a second set of contacts —
// so callers asserting idempotency re-replay a captured source payload rather
// than re-running SeedAll.
//
// The harness must be constructed for the SAME namespace as params.Namespace so
// the run's identifiers and cleanup scope line up.
func (s *seedAllRunner) run(ctx context.Context, params SeedParams) (SeedAllResult, error) {
	var res SeedAllResult
	gen := s.h.Generator()
	n := params.Counts.SeededContacts
	if n <= 0 {
		n = 1
	}

	for i := 0; i < n; i++ {
		// A Gmail-settled contact (email match). Hold the spec so the source
		// payload addresses the contact's exact identifiers.
		emailSpec := gen.Contact(factory.WithEmail())
		emailContact, err := s.h.SeedContact(ctx, emailSpec)
		if err != nil {
			return res, fmt.Errorf("seedall: seed email contact: %w", err)
		}
		gmailMsg := gen.GmailMessage(emailSpec, factory.MatchSeeded)
		if _, err := s.h.ReplayGmail(ctx, emailContact.ID, gmailMsg); err != nil {
			return res, fmt.Errorf("seedall: replay gmail: %w", err)
		}
		res.GmailContactIDs = append(res.GmailContactIDs, emailContact.ID.String())
		if res.GmailIdempotencyProbe == nil {
			res.GmailIdempotencyProbe = &GmailReplayProbe{ContactID: emailContact.ID, Spec: gmailMsg}
		}

		// A Telegram-settled contact (handle match).
		tgSpec := gen.Contact(factory.WithTelegram())
		tgContact, err := s.h.SeedContact(ctx, tgSpec)
		if err != nil {
			return res, fmt.Errorf("seedall: seed telegram contact: %w", err)
		}
		if _, err := s.h.ReplayTelegram(ctx, tgContact.ID, gen.TelegramMessage(tgSpec, factory.MatchSeeded)); err != nil {
			return res, fmt.Errorf("seedall: replay telegram: %w", err)
		}
		res.TelegramContactIDs = append(res.TelegramContactIDs, tgContact.ID.String())
	}
	return res, nil
}

// seedAllRunner binds SeedAll to a harness. It holds each generated ContactSpec
// so the source-payload factories can address the seeded contact's exact
// identifiers while the replay uses the persisted contact's id.
type seedAllRunner struct {
	h *Harness
}

// SeedAll runs the mode-(b) full-seed against the harness. Convenience wrapper.
func SeedAll(ctx context.Context, h *Harness, params SeedParams) (SeedAllResult, error) {
	return (&seedAllRunner{h: h}).run(ctx, params)
}
