//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	migrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// derivedStorageVersion is the golang-migrate version of the derived-storage
// migration (070). The down/up round-trip positions the clone here first so
// Steps(-1) rolls down 070 specifically, robust to later migrations landing
// above it.
const derivedStorageVersion = 70

// testEmbeddingDim is the fixed embedding dimensionality (vector(1536)). The
// stored vector must match the column dimension or the write is rejected.
const testEmbeddingDim = 1536

// makeTestVector builds a deterministic 1536-dim vector whose first element is
// fill, so two vectors with different fills are distinguishable on read-back.
func makeTestVector(fill float32) []float32 {
	v := make([]float32, testEmbeddingDim)
	for i := range v {
		v[i] = fill + float32(i)
	}
	return v
}

// TestDerivedStorage_EmbeddingRoundTrip stores an embedding, reads it back, and
// proves the composite-PK conflict overwrites the vector (not inserts a second
// row). target_id is no-FK polymorphic, so a fresh UUID needs no parent row.
func TestDerivedStorage_EmbeddingRoundTrip(t *testing.T) {
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	embeddingRepo := repository.NewEmbeddingRepository(database.Queries)

	targetID := uuid.New()
	const modelVersion = "text-embedding-3-small@v1"
	first := makeTestVector(1)

	require.NoError(t, embeddingRepo.UpsertEmbedding(ctx, repository.UpsertEmbeddingRequest{
		TargetKind:   repository.EmbeddingTargetInteraction,
		TargetID:     targetID,
		ModelVersion: modelVersion,
		Vector:       first,
	}))

	got, err := embeddingRepo.GetEmbedding(ctx, repository.EmbeddingTargetInteraction, targetID, modelVersion)
	require.NoError(t, err)
	assert.Equal(t, repository.EmbeddingTargetInteraction, got.TargetKind)
	assert.Equal(t, targetID, got.TargetID)
	assert.Equal(t, modelVersion, got.ModelVersion)
	assert.Equal(t, first, got.Vector)
	assert.False(t, got.ComputedAt.IsZero(), "computed_at defaulted to NOW()")

	// Composite-PK conflict: re-upsert the same key with a different vector. The
	// row is updated in place, not duplicated.
	second := makeTestVector(2)
	require.NoError(t, embeddingRepo.UpsertEmbedding(ctx, repository.UpsertEmbeddingRequest{
		TargetKind:   repository.EmbeddingTargetInteraction,
		TargetID:     targetID,
		ModelVersion: modelVersion,
		Vector:       second,
	}))
	updated, err := embeddingRepo.GetEmbedding(ctx, repository.EmbeddingTargetInteraction, targetID, modelVersion)
	require.NoError(t, err)
	assert.Equal(t, second, updated.Vector, "the conflicting upsert overwrites the vector")
}

// TestDerivedStorage_EmbeddingDeleteForTarget proves DeleteEmbeddingsForTarget
// wipes every model's embedding for one target while leaving other targets
// intact.
func TestDerivedStorage_EmbeddingDeleteForTarget(t *testing.T) {
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	embeddingRepo := repository.NewEmbeddingRepository(database.Queries)

	targetID := uuid.New()
	otherID := uuid.New()

	// Two models for the same target, plus one for an unrelated target.
	for _, mv := range []string{"model-a", "model-b"} {
		require.NoError(t, embeddingRepo.UpsertEmbedding(ctx, repository.UpsertEmbeddingRequest{
			TargetKind:   repository.EmbeddingTargetNode,
			TargetID:     targetID,
			ModelVersion: mv,
			Vector:       makeTestVector(1),
		}))
	}
	require.NoError(t, embeddingRepo.UpsertEmbedding(ctx, repository.UpsertEmbeddingRequest{
		TargetKind:   repository.EmbeddingTargetNode,
		TargetID:     otherID,
		ModelVersion: "model-a",
		Vector:       makeTestVector(1),
	}))

	require.NoError(t, embeddingRepo.DeleteEmbeddingsForTarget(ctx, repository.EmbeddingTargetNode, targetID))

	for _, mv := range []string{"model-a", "model-b"} {
		_, err := embeddingRepo.GetEmbedding(ctx, repository.EmbeddingTargetNode, targetID, mv)
		require.ErrorIs(t, err, db.ErrNotFound, "every model's embedding for the target is wiped")
	}
	// The unrelated target survives.
	_, err := embeddingRepo.GetEmbedding(ctx, repository.EmbeddingTargetNode, otherID, "model-a")
	require.NoError(t, err, "delete-for-target does not touch other targets")
}

// TestDerivedStorage_EmbeddingRejectsUnknownKind proves the target_kind CHECK
// constraint rejects a kind outside the closed enum.
func TestDerivedStorage_EmbeddingRejectsUnknownKind(t *testing.T) {
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	embeddingRepo := repository.NewEmbeddingRepository(database.Queries)

	err := embeddingRepo.UpsertEmbedding(ctx, repository.UpsertEmbeddingRequest{
		TargetKind:   "contact", // not in the closed CHECK enum
		TargetID:     uuid.New(),
		ModelVersion: "model-a",
		Vector:       makeTestVector(1),
	})
	require.Error(t, err, "the target_kind CHECK rejects a kind outside the enum")
}

// TestDerivedStorage_RelationshipSignalRoundTrip stores a signal, reads it
// back, proves the composite-PK conflict overwrites value + watermarks, lists
// every signal for the subject, and deletes them all. subject_node_id is a real
// FK→node, so the test mints a node first.
func TestDerivedStorage_RelationshipSignalRoundTrip(t *testing.T) {
	database, ctx := newSyntheticDB(t)
	t.Parallel()

	nodeRepo := repository.NewNodeRepository(database.Queries)
	signalRepo := repository.NewRelationshipSignalRepository(database.Queries)

	subjectID := uuid.New()
	_, err := nodeRepo.CreateNode(ctx, subjectID, repository.NodeTypePerson, "signal-subject")
	require.NoError(t, err)

	asOf := accelerated.GetCurrentTime().UTC()
	require.NoError(t, signalRepo.UpsertRelationshipSignal(ctx, repository.UpsertRelationshipSignalRequest{
		SubjectNodeID: subjectID,
		SignalKey:     "closeness",
		Value:         0.5,
		AsOf:          asOf,
		MethodVersion: "v1",
	}))

	got, err := signalRepo.GetRelationshipSignal(ctx, subjectID, "closeness")
	require.NoError(t, err)
	assert.Equal(t, subjectID, got.SubjectNodeID)
	assert.Equal(t, "closeness", got.SignalKey)
	assert.InDelta(t, 0.5, got.Value, 1e-9)
	assert.Equal(t, "v1", got.MethodVersion)
	assert.True(t, asOf.Equal(got.AsOf), "as_of round-trips")
	assert.False(t, got.ComputedAt.IsZero(), "computed_at defaulted to NOW()")

	// Composite-PK conflict: re-upsert the same (subject, key) with a new value
	// and method_version. The row is updated in place.
	require.NoError(t, signalRepo.UpsertRelationshipSignal(ctx, repository.UpsertRelationshipSignalRequest{
		SubjectNodeID: subjectID,
		SignalKey:     "closeness",
		Value:         0.9,
		AsOf:          asOf,
		MethodVersion: "v2",
	}))
	updated, err := signalRepo.GetRelationshipSignal(ctx, subjectID, "closeness")
	require.NoError(t, err)
	assert.InDelta(t, 0.9, updated.Value, 1e-9, "the conflicting upsert overwrites the value")
	assert.Equal(t, "v2", updated.MethodVersion, "the conflicting upsert overwrites method_version")

	// A second key for the same subject; ListSignalsForSubject returns both,
	// ordered by key.
	require.NoError(t, signalRepo.UpsertRelationshipSignal(ctx, repository.UpsertRelationshipSignalRequest{
		SubjectNodeID: subjectID,
		SignalKey:     "real_cadence_days",
		Value:         14,
		AsOf:          asOf,
		MethodVersion: "v1",
	}))
	signals, err := signalRepo.ListSignalsForSubject(ctx, subjectID)
	require.NoError(t, err)
	require.Len(t, signals, 2)
	assert.Equal(t, "closeness", signals[0].SignalKey)
	assert.Equal(t, "real_cadence_days", signals[1].SignalKey)

	// DeleteSignalsForSubject wipes them all.
	require.NoError(t, signalRepo.DeleteSignalsForSubject(ctx, subjectID))
	remaining, err := signalRepo.ListSignalsForSubject(ctx, subjectID)
	require.NoError(t, err)
	assert.Empty(t, remaining, "delete-for-subject wipes every signal")
}

// TestDerivedStorage_MigrationDownUp exercises the 070 down + up round-trip
// against an isolated clone (it rolls the schema down, so it cannot share the
// package DB). It proves both tables drop cleanly and re-create with the
// constraints intact (the target_kind CHECK is re-enforced).
func TestDerivedStorage_MigrationDownUp(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	// Migration-subject test: rolls the schema down, so it stays serial and uses an
	// isolated clone (never the shared package DB).

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)
	migrationsPath := getMigrationsPath()

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	embeddingRepo := repository.NewEmbeddingRepository(database.Queries)

	// The clone is template-migrated, so the embedding table is present up front:
	// an embedding round-trips.
	targetID := uuid.New()
	require.NoError(t, embeddingRepo.UpsertEmbedding(ctx, repository.UpsertEmbeddingRequest{
		TargetKind:   repository.EmbeddingTargetNode,
		TargetID:     targetID,
		ModelVersion: "before-rollback",
		Vector:       makeTestVector(1),
	}))

	m, err := migrate.New(fmt.Sprintf("file://%s", migrationsPath), cloneURL)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	// Position the clone at the derived-storage tip (070) FIRST, so Steps(-1) rolls
	// down 070 specifically — robust to later migrations being added above it.
	// Migrate(70) is a no-op today because the template clone is already at 70
	// (ErrNoChange); once 071+ lands it rolls the clone back down to 70 first.
	if err := m.Migrate(derivedStorageVersion); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		require.NoError(t, err, "position the clone at the derived-storage tip")
	}

	// Roll down ONE step: 070 down — both tables are dropped. A query against the
	// embedding table now errors (the relation is gone, not ErrNotFound).
	require.NoError(t, m.Steps(-1), "roll the derived-storage migration down one step")
	_, err = embeddingRepo.GetEmbedding(ctx, repository.EmbeddingTargetNode, targetID, "before-rollback")
	require.Error(t, err, "embedding table is dropped after the down migration")

	// Roll back up: the tables are recreated with the constraints intact. The old
	// row does NOT come back (the table was dropped + recreated), and a fresh
	// insert succeeds — but the target_kind CHECK still rejects a bad row, proving
	// the up migration reinstalled the constraint.
	require.NoError(t, m.Steps(1), "re-apply the derived storage")
	_, err = embeddingRepo.GetEmbedding(ctx, repository.EmbeddingTargetNode, targetID, "before-rollback")
	require.ErrorIs(t, err, db.ErrNotFound, "table drop+recreate does not restore the old row")

	require.NoError(t, embeddingRepo.UpsertEmbedding(ctx, repository.UpsertEmbeddingRequest{
		TargetKind:   repository.EmbeddingTargetNode,
		TargetID:     uuid.New(),
		ModelVersion: "after-rollback",
		Vector:       makeTestVector(1),
	}), "the recreated table accepts a valid insert")

	err = embeddingRepo.UpsertEmbedding(ctx, repository.UpsertEmbeddingRequest{
		TargetKind:   "contact", // not in the closed CHECK enum
		TargetID:     uuid.New(),
		ModelVersion: "after-rollback",
		Vector:       makeTestVector(1),
	})
	require.Error(t, err, "the recreated table re-enforces the target_kind CHECK")
}
