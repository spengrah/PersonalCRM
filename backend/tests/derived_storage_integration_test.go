//go:build integration_testdb

package tests

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	// Truncate to microseconds: as_of is a TIMESTAMPTZ (µs precision), so a Go
	// time carrying sub-µs nanoseconds would not round-trip exactly.
	asOf := accelerated.GetCurrentTime().UTC().Truncate(time.Microsecond)
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
