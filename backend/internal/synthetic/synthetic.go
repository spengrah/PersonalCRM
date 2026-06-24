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

// ValidateNamespace reports whether a namespace token is safe for the toolkit's
// prefix-based cleanup (lowercase alphanumerics + hyphens; no SQL LIKE
// metacharacters). NewHarness* enforce it at construction; re-exported so callers
// can validate up front.
func ValidateNamespace(namespace string) error {
	return replay.ValidateNamespace(namespace)
}

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

// NewHarnessWithDBForNamespace builds a non-test harness for an EXPLICIT
// namespace + seed (the crm-admin profile entrypoints). Re-exported so the
// entrypoints can pin a stable per-profile namespace for a reproducible seed
// world. The success path calls Harness.Quiesce (seed-and-leave); the error path
// calls the returned teardown closure (stop client + clean the partial world).
func NewHarnessWithDBForNamespace(ctx context.Context, database *db.Database, namespace string, seed uint64) (*Harness, func(context.Context) error, error) {
	return replay.NewHarnessWithDBForNamespace(ctx, database, namespace, seed)
}

// Counts tunes the per-entity volume a profile produces. The per-profile
// distribution knobs the profiles read; only the knobs a profile actually
// consumes are present (no distribution DSL). DefaultParams supplies the small
// "minimal-scoped" shape.
type Counts struct {
	SeededContacts    int // contacts seeded with a matching identifier per source
	UnmatchedExternal int // import-candidate (unmatched external_contact) rows
	StrandedTelegram  int // stranded telegram_message rows (matched_contact_id IS NULL)
	StrandedMessages  int // stranded messages_message (iMessage) rows
	UnmatchedCalendar int // calendar_event rows with an unmatched attendee
	OrphanMeetingNote int // orphan_needs_review meeting_note rows
	SeededAssertions  int // graph (SP1) fact assertions seeded on the first contact node
}

// SeedParams is the profile/volume seam consumed by the seed orchestration.
// Namespace + Seed make the run deterministic and isolation-scoped; Profile
// selects which world to build.
type SeedParams struct {
	Namespace string
	Seed      uint64
	Profile   Profile
	Counts    Counts
}

// DefaultParams is the small, namespaced "minimal-scoped" shape so the core is
// usable and testable out of the box. Profile is set EXPLICITLY (the zero value
// of a Profile is "", which is an unknown/error profile, NOT minimal-scoped).
func DefaultParams() SeedParams {
	return SeedParams{
		Namespace: "seedall",
		Seed:      factory.DefaultSeed,
		Profile:   ProfileMinimalScoped,
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
