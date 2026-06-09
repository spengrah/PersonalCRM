package tests

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkDefShapeRe asserts the rendered constraint is EXACTLY the
// `CHECK ((source = ANY (ARRAY[...])))` form PostgreSQL 16 produces for a
// single source-membership predicate. The pattern is fully anchored
// (^...$) so a widened constraint that bolts an extra permissive clause
// onto the ANY (e.g. `... OR source = 'x'`) fails the shape match instead
// of slipping through a substring search — the literal parser only sees
// the ARRAY members, so without this anchor an added OR-clause would
// widen the live set undetected. If a future PG ever renders it as
// IN (...) the test also fails loudly here rather than mis-parsing.
var checkDefShapeRe = regexp.MustCompile(`^CHECK \(\(source = ANY \(ARRAY\[[^]]*\]\)\)\)$`)

// checkDefLiteralRe extracts each quoted source literal from the rendered
// ARRAY[...] — every element is `'value'::text`.
var checkDefLiteralRe = regexp.MustCompile(`'([^']+)'::text`)

// TestInteractionSourceCheck_AgreesWithDescriptorAndConstants is the
// live-DB half of the interaction.source agreement: the CHECK constraint
// is kept (not migrated to a lookup table), so this test pins it. It
// parses the actual interaction_source_check membership from the running
// PostgreSQL via
// pg_get_constraintdef and asserts, set-for-set:
//
//	live CHECK set  ==  all repository.InteractionSource* constants
//
// in BOTH directions (a constant missing from the DB enum AND an
// accidental DB widening beyond the constants both fail), then asserts
// every daemon-push family that writes interactions carries an
// interactionSource present in the live CHECK set. Closes the loop three
// ways: descriptor ↔ Go constants ↔ live DB CHECK.
func TestInteractionSourceCheck_AgreesWithDescriptorAndConstants(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()

	// CI runs against a bare PostgreSQL — run migrations before opening
	// the pool so the interaction table + its CHECK exist. (TestMain in
	// this package also runs them; this is idempotent and keeps the test
	// self-contained.)
	require.NoError(t, db.RunMigrations(ctx, databaseURL, getMigrationsPath()))

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	interactionRepo := repository.NewInteractionRepository(database.Queries)

	// --- Parse the live CHECK membership. ---
	def, err := interactionRepo.GetInteractionSourceCheckDef(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, def, "interaction_source_check definition must be non-empty")
	require.Regexp(t, checkDefShapeRe, def,
		"unexpected CHECK render shape (parser expects source = ANY (ARRAY[...])): %q", def)

	matches := checkDefLiteralRe.FindAllStringSubmatch(def, -1)
	require.NotEmpty(t, matches, "expected at least one quoted source literal in: %q", def)
	liveSet := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		liveSet[m[1]] = struct{}{}
	}

	// --- Assert live set == all 9 InteractionSource* constants (both
	// directions). ---
	allConstants := []string{
		repository.InteractionSourceManual,
		repository.InteractionSourceGCal,
		repository.InteractionSourceTodoist,
		repository.InteractionSourceTelegram,
		repository.InteractionSourceMessages,
		repository.InteractionSourceAnarlogSessions,
		repository.InteractionSourcePhoneCalls,
		repository.InteractionSourceEmail,
		repository.InteractionSourceGChat,
	}
	constantSet := make(map[string]struct{}, len(allConstants))
	for _, c := range allConstants {
		constantSet[c] = struct{}{}
	}
	for c := range constantSet {
		_, ok := liveSet[c]
		assert.Truef(t, ok, "InteractionSource* constant %q missing from live CHECK set", c)
	}
	for s := range liveSet {
		_, ok := constantSet[s]
		assert.Truef(t, ok, "live CHECK source %q has no matching InteractionSource* constant (DB widened?)", s)
	}

	// --- Assert every push family that writes interactions carries an
	// interactionSource present in the live CHECK set (descriptor → CHECK
	// link). Read through the PRODUCTION accessor — backend/tests links
	// the production service archive and cannot see test-only exports. ---
	for _, v := range service.DaemonFamilyViews() {
		if !v.WritesInteractions {
			continue
		}
		_, ok := liveSet[v.InteractionSource]
		assert.Truef(t, ok,
			"family %q interactionSource %q is not in the live interaction_source_check set",
			v.Name, v.InteractionSource)
	}
}

// TestInteractionSourceCheck_AcceptsPhoneCalls is a belt-and-suspenders
// acceptance smoke-insert for one push source (phone_calls), reusing the
// data-loss-guard cleanup convention from
// interaction_source_messages_check_test.go: HARD-delete the seeded row
// before closing the DB handle (soft-delete is insufficient — the
// down-migration data-loss guard counts rows regardless of deleted_at).
func TestInteractionSourceCheck_AcceptsPhoneCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	suffix := syntheticNS(t)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Test PhoneCalls Source CHECK " + suffix,
	})
	require.NoError(t, err)
	defer func() {
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourcePhoneCalls, "phone-calls-test-"+suffix+"%")
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	}()

	ref := "phone-calls-test-" + suffix
	interaction, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourcePhoneCalls,
		SourceRef:  &ref,
		OccurredAt: accelerated.GetCurrentTime().Truncate(time.Microsecond),
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionSourcePhoneCalls, interaction.Source)
}
