//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"os"
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

// expectedCuratedPredicates is the exact set of curated-core predicate keys the
// seed migration installs (plan §3). The seed assertions check presence/values
// of these specific keys rather than a global count.
var expectedCuratedPredicates = []string{
	"lives_in", "home_address", "works_at", "job_title", "birthday",
	"partner_of", "parent_of", "child_of", "sibling_of",
	"grandparent_of", "grandchild_of", "aunt_uncle_of", "niece_nephew_of", "cousin_of",
	"health_condition", "interested_in", "preference", "how_met", "tagged_as",
	"knows", "introduced_by",
	"job_seeking", "on_sabbatical", "traveling", "occurrence", "within",
}

// expectedCuratedSubtypes is the exact set of entity_type subtype keys the seed
// migration installs (plan §3).
var expectedCuratedSubtypes = []string{"organization", "place", "topic", "tag"}

func TestPredicateCatalog_Seed_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	database, ctx := graphTestDB(t)
	predicateRepo := repository.NewPredicateRepository(database.Queries)
	entityRepo := repository.NewEntityRepository(database.Queries)

	t.Run("seed installs exactly the expected curated predicate keys", func(t *testing.T) {
		t.Parallel()
		curated, err := predicateRepo.ListCurated(ctx)
		require.NoError(t, err)

		got := make(map[string]repository.Predicate, len(curated))
		for _, p := range curated {
			got[p.Key] = p
		}
		for _, key := range expectedCuratedPredicates {
			_, ok := got[key]
			assert.Truef(t, ok, "curated catalog must contain %q", key)
		}
		// Every curated row really is status='curated' (ListCurated filters on it).
		for _, p := range curated {
			assert.Equal(t, repository.PredicateStatusCurated, p.Status, p.Key)
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

	t.Run("spot-check sensitive predicate fields", func(t *testing.T) {
		t.Parallel()

		// lives_in: edge → person→place, single, mutable, year bucket, auto-apply.
		livesIn, err := predicateRepo.GetPredicate(ctx, "lives_in")
		require.NoError(t, err)
		assert.Equal(t, repository.PredicateKindEdge, livesIn.Kind)
		assert.Equal(t, "person", livesIn.SubjectType)
		require.NotNil(t, livesIn.ObjectType)
		assert.Equal(t, "place", *livesIn.ObjectType)
		assert.Nil(t, livesIn.ValueType)
		assert.Equal(t, repository.PredicateCardinalitySingle, livesIn.Cardinality)
		assert.Equal(t, repository.PredicateTemporalMutable, livesIn.TemporalProfile)
		assert.Equal(t, repository.PredicateBucketYear, livesIn.PropositionBucket)
		assert.Equal(t, repository.PredicateReviewAutoIfConfident, livesIn.DefaultReviewPolicy)
		require.NotNil(t, livesIn.BaseRateDays)
		assert.Equal(t, int32(2190), *livesIn.BaseRateDays)
		assert.Nil(t, livesIn.Embedding, "seeded predicates ship with no embedding")

		// birthday: permanent fact, none bucket, date value.
		birthday, err := predicateRepo.GetPredicate(ctx, "birthday")
		require.NoError(t, err)
		assert.Equal(t, repository.PredicateKindFact, birthday.Kind)
		require.NotNil(t, birthday.ValueType)
		assert.Equal(t, repository.PredicateValueTypeDate, *birthday.ValueType)
		assert.Nil(t, birthday.ObjectType)
		assert.Equal(t, repository.PredicateTemporalPermanent, birthday.TemporalProfile)
		assert.Equal(t, repository.PredicateBucketNone, birthday.PropositionBucket)

		// health_condition: always-confirm, multi, text fact.
		health, err := predicateRepo.GetPredicate(ctx, "health_condition")
		require.NoError(t, err)
		assert.Equal(t, repository.PredicateReviewAlwaysConfirm, health.DefaultReviewPolicy)
		assert.Equal(t, repository.PredicateCardinalityMulti, health.Cardinality)
		require.NotNil(t, health.ValueType)
		assert.Equal(t, repository.PredicateValueTypeText, *health.ValueType)

		// partner_of: symmetric, single, always-confirm.
		partner, err := predicateRepo.GetPredicate(ctx, "partner_of")
		require.NoError(t, err)
		assert.True(t, partner.Symmetric, "partner_of is symmetric")
		assert.Equal(t, repository.PredicateCardinalitySingle, partner.Cardinality)
		assert.Equal(t, repository.PredicateReviewAlwaysConfirm, partner.DefaultReviewPolicy)

		// within: the place→place hierarchy edge — subject is an entity subtype,
		// NOT person (the first such seeded predicate).
		within, err := predicateRepo.GetPredicate(ctx, "within")
		require.NoError(t, err)
		assert.Equal(t, repository.PredicateKindEdge, within.Kind)
		assert.Equal(t, "place", within.SubjectType)
		require.NotNil(t, within.ObjectType)
		assert.Equal(t, "place", *within.ObjectType)
		assert.Equal(t, repository.PredicateCardinalitySingle, within.Cardinality)

		// occurrence: bounded text fact with a typical duration prior.
		occ, err := predicateRepo.GetPredicate(ctx, "occurrence")
		require.NoError(t, err)
		assert.Equal(t, repository.PredicateTemporalBounded, occ.TemporalProfile)
		require.NotNil(t, occ.TypicalDurationDays)
		assert.Equal(t, int32(7), *occ.TypicalDurationDays)
	})

	t.Run("inverse pairs resolve in both directions", func(t *testing.T) {
		t.Parallel()
		pairs := [][2]string{
			{"parent_of", "child_of"},
			{"grandparent_of", "grandchild_of"},
			{"aunt_uncle_of", "niece_nephew_of"},
		}
		for _, pair := range pairs {
			a, err := predicateRepo.GetPredicate(ctx, pair[0])
			require.NoError(t, err)
			require.NotNilf(t, a.InversePredicate, "%s must have an inverse", pair[0])
			assert.Equalf(t, pair[1], *a.InversePredicate, "%s inverse", pair[0])

			b, err := predicateRepo.GetPredicate(ctx, pair[1])
			require.NoError(t, err)
			require.NotNilf(t, b.InversePredicate, "%s must have an inverse", pair[1])
			assert.Equalf(t, pair[0], *b.InversePredicate, "%s inverse", pair[1])
		}

		// Symmetric and non-paired predicates carry no inverse.
		sibling, err := predicateRepo.GetPredicate(ctx, "sibling_of")
		require.NoError(t, err)
		assert.Nil(t, sibling.InversePredicate, "symmetric sibling_of has no inverse_predicate")
		livesIn, err := predicateRepo.GetPredicate(ctx, "lives_in")
		require.NoError(t, err)
		assert.Nil(t, livesIn.InversePredicate)
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
			Status:              repository.PredicateStatusProvisional,
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
			Status:              repository.PredicateStatusProvisional,
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
		Status:              repository.PredicateStatusProvisional,
	})
	require.NoError(t, err)

	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	// Roll down ONE step: 066 (the seed) down. The predicate table still exists.
	require.NoError(t, m.Steps(-1), "roll the seed migration down one step")

	// Every seeded curated key is gone...
	for _, key := range expectedCuratedPredicates {
		_, err := predicateRepo.GetPredicate(ctx, key)
		require.ErrorIsf(t, err, db.ErrNotFound, "seed-down must remove curated key %q", key)
	}
	entityRepo := repository.NewEntityRepository(database.Queries)
	for _, key := range expectedCuratedSubtypes {
		_, err := entityRepo.GetEntityType(ctx, key)
		require.ErrorIsf(t, err, db.ErrNotFound, "seed-down must remove curated subtype %q", key)
	}
	// ...but the provisional row survives (seed-down deletes only seeded keys).
	survivor, err := predicateRepo.GetPredicate(ctx, provisionalKey)
	require.NoError(t, err, "seed-down must NOT touch a provisional predicate")
	assert.Equal(t, provisionalKey, survivor.Key)

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
