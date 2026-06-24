//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Predicate-catalog integration coverage. The curated seed is GLOBAL data (not
// namespaced), so the seed-content assertions read the shared template DB's
// global catalog and assert on specific keys/values — never on global counts a
// sibling test could perturb. The provisional-create + CHECK-rejection sub-tests
// use namespaced keys with scoped cleanup. The migration up/down round-trip runs
// against an ISOLATED per-test clone (NewEphemeralClone), never the shared DB,
// because it rolls the schema down.

// seedPredicateCatalogVersion is the golang-migrate version of the predicate
// catalog SEED migration (066_seed_predicate_catalog). The down/up round-trip
// test positions the clone at this version before its relative roll-down so it is
// robust to later migrations (067+) being added above it.
const seedPredicateCatalogVersion = 66

// i32p returns a pointer to an int32 literal, for the nullable prior fields.
func i32p(v int32) *int32 { return &v }

// expectedCuratedPredicate captures the FULL curated-seed contract for one
// predicate: every per-predicate field the table-driven test verifies against
// the live seed (so a metadata typo on any predicate fails the test). The two
// nullable priors are pointers (nil = the seed leaves them NULL).
type expectedCuratedPredicate struct {
	kind           string
	subjectType    string
	objectType     string // edges only; "" for facts
	valueType      string // facts only; "" for edges
	cardinality    string
	symmetric      bool
	inverse        string // "" when none
	temporal       string
	baseRateDays   *int32 // nil = NULL
	typicalDurDays *int32 // nil = NULL
	salience       int16
	reviewPolicy   string
	bucket         string
}

// expectedCuratedCatalog is the exact curated seed the migration installs. The
// test asserts the live curated set equals these keys EXACTLY and that every
// row's full field set matches.
var expectedCuratedCatalog = map[string]expectedCuratedPredicate{
	"lives_in":         {"edge", "person", "place", "", "single", false, "", "mutable", i32p(2190), nil, 60, "auto-if-confident", "year"},
	"home_address":     {"fact", "person", "", "text", "single", false, "", "mutable", i32p(2190), nil, 45, "auto-if-confident", "year"},
	"works_at":         {"edge", "person", "organization", "", "single", false, "", "mutable", i32p(1460), nil, 60, "auto-if-confident", "year"},
	"job_title":        {"fact", "person", "", "text", "single", false, "", "mutable", i32p(1095), nil, 45, "auto-if-confident", "year"},
	"birthday":         {"fact", "person", "", "date", "single", false, "", "permanent", nil, nil, 85, "auto-if-confident", "none"},
	"partner_of":       {"edge", "person", "person", "", "single", true, "", "mutable", nil, nil, 85, "always-confirm", "none"},
	"parent_of":        {"edge", "person", "person", "", "multi", false, "child_of", "permanent", nil, nil, 80, "always-confirm", "none"},
	"child_of":         {"edge", "person", "person", "", "multi", false, "parent_of", "permanent", nil, nil, 80, "always-confirm", "none"},
	"sibling_of":       {"edge", "person", "person", "", "multi", true, "", "permanent", nil, nil, 80, "always-confirm", "none"},
	"grandparent_of":   {"edge", "person", "person", "", "multi", false, "grandchild_of", "permanent", nil, nil, 55, "auto-if-confident", "none"},
	"grandchild_of":    {"edge", "person", "person", "", "multi", false, "grandparent_of", "permanent", nil, nil, 55, "auto-if-confident", "none"},
	"aunt_uncle_of":    {"edge", "person", "person", "", "multi", false, "niece_nephew_of", "permanent", nil, nil, 55, "auto-if-confident", "none"},
	"niece_nephew_of":  {"edge", "person", "person", "", "multi", false, "aunt_uncle_of", "permanent", nil, nil, 55, "auto-if-confident", "none"},
	"cousin_of":        {"edge", "person", "person", "", "multi", true, "", "permanent", nil, nil, 55, "auto-if-confident", "none"},
	"health_condition": {"fact", "person", "", "text", "multi", false, "", "mutable", nil, nil, 80, "always-confirm", "none"},
	"interested_in":    {"edge", "person", "topic", "", "multi", false, "", "mutable", i32p(3650), nil, 45, "auto-if-confident", "none"},
	"preference":       {"fact", "person", "", "text", "multi", false, "", "mutable", nil, nil, 35, "auto-if-confident", "none"},
	"how_met":          {"fact", "person", "", "text", "single", false, "", "permanent", nil, nil, 60, "auto-if-confident", "none"},
	"tagged_as":        {"edge", "person", "tag", "", "multi", false, "", "permanent", nil, nil, 50, "auto-if-confident", "none"},
	"knows":            {"edge", "person", "person", "", "multi", true, "", "mutable", i32p(3650), nil, 55, "auto-if-confident", "none"},
	"introduced_by":    {"edge", "person", "person", "", "single", false, "", "permanent", nil, nil, 55, "auto-if-confident", "none"},
	"job_seeking":      {"fact", "person", "", "bool", "single", false, "", "bounded", nil, i32p(180), 60, "auto-if-confident", "day"},
	"on_sabbatical":    {"fact", "person", "", "bool", "single", false, "", "bounded", nil, i32p(180), 55, "auto-if-confident", "day"},
	"traveling":        {"fact", "person", "", "bool", "single", false, "", "bounded", nil, i32p(30), 50, "auto-if-confident", "day"},
	"occurrence":       {"fact", "person", "", "text", "multi", false, "", "bounded", nil, i32p(7), 80, "always-confirm", "day"},
	"within":           {"edge", "place", "place", "", "single", false, "", "permanent", nil, nil, 30, "auto-if-confident", "none"},
}

// expectedCuratedSubtypes is the exact set of entity_type subtype keys the seed
// migration installs.
var expectedCuratedSubtypes = []string{"organization", "place", "topic", "tag"}

func TestPredicateCatalog_Seed_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	database, ctx := graphTestDB(t)
	predicateRepo := repository.NewPredicateRepository(database.Queries)
	entityRepo := repository.NewEntityRepository(database.Queries)

	t.Run("curated catalog is EXACTLY the expected seeded set", func(t *testing.T) {
		t.Parallel()
		curated, err := predicateRepo.ListCurated(ctx)
		require.NoError(t, err)

		// The curated catalog is global, seed-only data (provisional rows minted at
		// runtime are excluded from ListCurated), so the live curated set must equal
		// the expected key set EXACTLY — an extra row is as much a seed bug as a
		// missing one.
		gotKeys := make([]string, 0, len(curated))
		for _, p := range curated {
			gotKeys = append(gotKeys, p.Key)
		}
		wantKeys := make([]string, 0, len(expectedCuratedCatalog))
		for k := range expectedCuratedCatalog {
			wantKeys = append(wantKeys, k)
		}
		sort.Strings(gotKeys)
		sort.Strings(wantKeys)
		assert.Equal(t, wantKeys, gotKeys, "curated catalog must be EXACTLY the seeded set")
	})

	t.Run("every seeded predicate's full field set matches the contract", func(t *testing.T) {
		t.Parallel()
		// Table-driven over EVERY curated predicate: kind/payload typing,
		// cardinality, the symmetric flag, inverse-pair linkage, temporal profile,
		// the two soft priors, salience, review policy, and proposition bucket are
		// all verified per row (not a subset spot-check), so a seed metadata typo
		// on any predicate fails here.
		for key, want := range expectedCuratedCatalog {
			p, err := predicateRepo.GetPredicate(ctx, key)
			require.NoErrorf(t, err, "predicate %q must exist", key)

			assert.Equalf(t, repository.PredicateStatusCurated, p.Status, "%s status", key)
			assert.Equalf(t, want.kind, p.Kind, "%s kind", key)
			assert.Equalf(t, want.subjectType, p.SubjectType, "%s subject_type", key)
			assert.Equalf(t, want.cardinality, p.Cardinality, "%s cardinality", key)
			assert.Equalf(t, want.symmetric, p.Symmetric, "%s symmetric", key)
			assert.Equalf(t, want.temporal, p.TemporalProfile, "%s temporal_profile", key)
			assert.Equalf(t, want.salience, p.DefaultSalience, "%s default_salience", key)
			assert.Equalf(t, want.bucket, p.PropositionBucket, "%s proposition_bucket", key)
			assert.Equalf(t, want.reviewPolicy, p.DefaultReviewPolicy, "%s default_review_policy", key)
			assert.Nilf(t, p.Embedding, "%s ships with no embedding", key)

			// Soft priors (nil = the seed leaves the column NULL).
			assert.Equalf(t, want.baseRateDays, p.BaseRateDays, "%s base_rate_days", key)
			assert.Equalf(t, want.typicalDurDays, p.TypicalDurationDays, "%s typical_duration_days", key)

			// Payload typing: edges carry an object_type and no value_type; facts
			// carry a value_type and no object_type (the kind/payload CHECK).
			if want.kind == repository.PredicateKindEdge {
				require.NotNilf(t, p.ObjectType, "%s edge must have object_type", key)
				assert.Equalf(t, want.objectType, *p.ObjectType, "%s object_type", key)
				assert.Nilf(t, p.ValueType, "%s edge must have no value_type", key)
			} else {
				require.NotNilf(t, p.ValueType, "%s fact must have value_type", key)
				assert.Equalf(t, want.valueType, *p.ValueType, "%s value_type", key)
				assert.Nilf(t, p.ObjectType, "%s fact must have no object_type", key)
			}

			// Inverse linkage (both directions are checked because every paired key
			// is itself a row in the table).
			if want.inverse == "" {
				assert.Nilf(t, p.InversePredicate, "%s must have no inverse", key)
			} else {
				require.NotNilf(t, p.InversePredicate, "%s must have an inverse", key)
				assert.Equalf(t, want.inverse, *p.InversePredicate, "%s inverse", key)
			}
		}
	})

	t.Run("seed installs the 4 curated entity subtypes (place has resolution config)", func(t *testing.T) {
		t.Parallel()
		for _, key := range expectedCuratedSubtypes {
			et, err := entityRepo.GetEntityType(ctx, key)
			require.NoErrorf(t, err, "entity_type %q must exist", key)
			assert.Equal(t, repository.EntityTypeStatusCurated, et.Status, key)
		}
		// place carries the hierarchical / synonym-normalization resolution config.
		place, err := entityRepo.GetEntityType(ctx, "place")
		require.NoError(t, err)
		assert.JSONEq(t, `{"hierarchical": true, "normalize_synonyms": true}`, string(place.ResolutionConfig))
		// The others default to '{}'.
		org, err := entityRepo.GetEntityType(ctx, "organization")
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(org.ResolutionConfig))
	})

	t.Run("kind/payload CHECK rejects an edge predicate carrying a value_type", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		support := repository.NewSyntheticSupportRepository(database.Queries)
		t.Cleanup(func() { _, _ = support.DeletePredicatesByKeyPrefix(ctx, gen.Prefix()) })

		object := "person"
		value := repository.PredicateValueTypeText
		// An edge with BOTH object_type AND value_type violates predicate_kind_payload.
		_, err := predicateRepo.CreateProvisional(ctx, repository.CreatePredicateRequest{
			Key:                 gen.Prefix() + "bad-edge",
			Kind:                repository.PredicateKindEdge,
			SubjectType:         "person",
			ObjectType:          &object,
			ValueType:           &value,
			Cardinality:         repository.PredicateCardinalityMulti,
			TemporalProfile:     repository.PredicateTemporalMutable,
			DefaultReviewPolicy: repository.PredicateReviewAutoIfConfident,
			PropositionBucket:   repository.PredicateBucketDay,
		})
		require.Error(t, err, "edge + value_type must violate the kind/payload CHECK")
	})

	t.Run("CreateProvisional mints a fact predicate with NULL embedding", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		support := repository.NewSyntheticSupportRepository(database.Queries)
		t.Cleanup(func() { _, _ = support.DeletePredicatesByKeyPrefix(ctx, gen.Prefix()) })

		value := repository.PredicateValueTypeText
		key := gen.Prefix() + "fav-color"
		created, err := predicateRepo.CreateProvisional(ctx, repository.CreatePredicateRequest{
			Key:                 key,
			Kind:                repository.PredicateKindFact,
			SubjectType:         "person",
			ValueType:           &value,
			Cardinality:         repository.PredicateCardinalitySingle,
			TemporalProfile:     repository.PredicateTemporalMutable,
			DefaultSalience:     40,
			DefaultReviewPolicy: repository.PredicateReviewAutoIfConfident,
			PropositionBucket:   repository.PredicateBucketDay,
			Description:         "synthetic provisional predicate",
		})
		require.NoError(t, err)
		assert.Equal(t, key, created.Key)
		assert.Equal(t, repository.PredicateStatusProvisional, created.Status)
		assert.Nil(t, created.Embedding, "provisional minting carries no embedding")

		got, err := predicateRepo.GetPredicate(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, repository.PredicateStatusProvisional, got.Status)
		require.NotNil(t, got.ValueType)
		assert.Equal(t, value, *got.ValueType)
	})
}

// TestPredicateCatalog_MigrationDownUp exercises the 065/066 down + up
// round-trip and proves the seed-down removes ONLY the seeded curated keys (a
// provisional row inserted beforehand survives the seed-down). It runs against
// an isolated clone because it rolls the schema down.
func TestPredicateCatalog_MigrationDownUp(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	// Migration-subject test: rolls the schema down, so it stays serial and uses
	// an isolated clone (never the shared package DB).

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	migrationsPath := getMigrationsPath()

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	predicateRepo := repository.NewPredicateRepository(database.Queries)

	// The clone is template-migrated, so the seed is present up front.
	_, err = predicateRepo.GetPredicate(ctx, "lives_in")
	require.NoError(t, err, "fully-migrated clone has the seeded catalog")

	// Insert a provisional predicate that must SURVIVE the seed-down (the down
	// deletes only the seeded curated keys by name, not provisional rows).
	value := repository.PredicateValueTypeText
	const provisionalKey = "test-provisional-survivor"
	_, err = predicateRepo.CreateProvisional(ctx, repository.CreatePredicateRequest{
		Key:                 provisionalKey,
		Kind:                repository.PredicateKindFact,
		SubjectType:         "person",
		ValueType:           &value,
		Cardinality:         repository.PredicateCardinalityMulti,
		TemporalProfile:     repository.PredicateTemporalMutable,
		DefaultReviewPolicy: repository.PredicateReviewAutoIfConfident,
		PropositionBucket:   repository.PredicateBucketDay,
	})
	require.NoError(t, err)

	// Insert a provisional EDGE whose inverse_predicate points at a SEEDED key.
	// The seed-down must clear THIS link (not just the seeded inverse-pair links)
	// before deleting the seeded rows, or the restrict self-FK blocks rollback.
	const linkedProvisionalKey = "test-provisional-linked"
	parentKey := "parent_of"
	objectPerson := "person"
	_, err = predicateRepo.CreateProvisional(ctx, repository.CreatePredicateRequest{
		Key:                 linkedProvisionalKey,
		Kind:                repository.PredicateKindEdge,
		SubjectType:         "person",
		ObjectType:          &objectPerson,
		Cardinality:         repository.PredicateCardinalityMulti,
		InversePredicate:    &parentKey,
		TemporalProfile:     repository.PredicateTemporalPermanent,
		DefaultReviewPolicy: repository.PredicateReviewAutoIfConfident,
		PropositionBucket:   repository.PredicateBucketNone,
	})
	require.NoError(t, err)

	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	// Position at the seed migration (066) as the tip BEFORE the relative roll-down,
	// so this test is independent of however many later migrations exist above it
	// (e.g. 067+). Without this, m.Steps(-1) would roll down the highest migration,
	// not the seed. Migrating down to 066 leaves the predicate table + seed intact.
	require.NoError(t, m.Migrate(seedPredicateCatalogVersion), "position the clone at the seed migration tip")

	// Roll down ONE step: 066 (the seed) down. The predicate table still exists.
	require.NoError(t, m.Steps(-1), "roll the seed migration down one step")

	// Every seeded curated key is gone...
	for key := range expectedCuratedCatalog {
		_, err := predicateRepo.GetPredicate(ctx, key)
		require.ErrorIsf(t, err, db.ErrNotFound, "seed-down must remove curated key %q", key)
	}
	entityRepo := repository.NewEntityRepository(database.Queries)
	for _, key := range expectedCuratedSubtypes {
		_, err := entityRepo.GetEntityType(ctx, key)
		require.ErrorIsf(t, err, db.ErrNotFound, "seed-down must remove curated subtype %q", key)
	}
	// ...but the provisional rows survive (seed-down deletes only seeded keys).
	survivor, err := predicateRepo.GetPredicate(ctx, provisionalKey)
	require.NoError(t, err, "seed-down must NOT touch a provisional predicate")
	assert.Equal(t, provisionalKey, survivor.Key)

	// The provisional edge that pointed its inverse at a seeded key survives too:
	// the seed-down cleared its inverse link (so the seeded-row delete didn't trip
	// the restrict self-FK) but kept the row itself.
	linked, err := predicateRepo.GetPredicate(ctx, linkedProvisionalKey)
	require.NoError(t, err, "seed-down must NOT delete a provisional row linked to a seeded key")
	assert.Nil(t, linked.InversePredicate, "seed-down clears the inverse link that pointed at a seeded key")

	// Roll down a second step: 065 (the table) down — the predicate table is now
	// dropped. A query against it errors (not ErrNotFound — the relation is gone).
	require.NoError(t, m.Steps(-1), "roll the predicate table down one step")
	_, err = predicateRepo.GetPredicate(ctx, provisionalKey)
	require.Error(t, err, "predicate table is dropped after the table-down migration")

	// Roll both back up: the table is recreated and the seed is reinstalled. The
	// provisional row does NOT come back (the table was dropped + recreated).
	require.NoError(t, m.Steps(2), "re-apply the predicate table + seed")
	reseeded, err := predicateRepo.GetPredicate(ctx, "lives_in")
	require.NoError(t, err, "up migration reinstalls the seed")
	assert.Equal(t, "lives_in", reseeded.Key)
	_, err = predicateRepo.GetPredicate(ctx, provisionalKey)
	require.ErrorIs(t, err, db.ErrNotFound, "table drop+recreate does not restore the provisional row")
}
