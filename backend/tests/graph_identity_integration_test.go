//go:build integration_testdb

package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Graph (SP1) identity-table round-trips against a real DB. Each sub-test is
// namespace-scoped via the lightweight factory generator (migrationGenerator)
// and cleans up its own node closure (entity/venue cascade from node) so the
// shared test DB stays isolated under t.Parallel().

func graphTestDB(t *testing.T) (*db.Database, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	t.Cleanup(database.Close)
	return database, ctx
}

func TestGraphIdentity_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	database, ctx := graphTestDB(t)
	nodeRepo := repository.NewNodeRepository(database.Queries)
	entityRepo := repository.NewEntityRepository(database.Queries)
	venueRepo := repository.NewVenueRepository(database.Queries)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	t.Run("node create/get/soft-delete round-trip", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		spec := gen.Node(repository.NodeTypePerson)
		id := uuid.New()
		created, err := nodeRepo.CreateNode(ctx, id, spec.Type, spec.CanonicalLabel)
		require.NoError(t, err)
		assert.Equal(t, id, created.ID)
		assert.Equal(t, repository.NodeTypePerson, created.Type)
		assert.Equal(t, spec.CanonicalLabel, created.CanonicalLabel)
		assert.Nil(t, created.DeletedAt)

		got, err := nodeRepo.GetNode(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, id, got.ID)

		// Soft-delete removes it from the live GetNode read but keeps it
		// resolvable via GetNodeIncludingDeleted.
		require.NoError(t, nodeRepo.SoftDeleteNode(ctx, id))
		_, err = nodeRepo.GetNode(ctx, id)
		require.ErrorIs(t, err, db.ErrNotFound)

		incl, err := nodeRepo.GetNodeIncludingDeleted(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, incl.DeletedAt)
	})

	t.Run("update canonical label", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		id := uuid.New()
		spec := gen.Node(repository.NodeTypePerson)
		_, err := nodeRepo.CreateNode(ctx, id, spec.Type, spec.CanonicalLabel)
		require.NoError(t, err)

		newLabel := gen.Prefix() + "renamed"
		require.NoError(t, nodeRepo.UpdateNodeCanonicalLabel(ctx, id, newLabel))
		got, err := nodeRepo.GetNode(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, newLabel, got.CanonicalLabel)
	})

	t.Run("merged_into set + read tombstones the loser", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		winnerID, loserID := uuid.New(), uuid.New()
		ws := gen.Node(repository.NodeTypePerson)
		ls := gen.Node(repository.NodeTypePerson)
		_, err := nodeRepo.CreateNode(ctx, winnerID, ws.Type, ws.CanonicalLabel)
		require.NoError(t, err)
		_, err = nodeRepo.CreateNode(ctx, loserID, ls.Type, ls.CanonicalLabel)
		require.NoError(t, err)

		require.NoError(t, nodeRepo.SetNodeMergedInto(ctx, loserID, winnerID))

		// The loser is tombstoned (dropped from live reads) and points at the
		// winner via merged_into.
		_, err = nodeRepo.GetNode(ctx, loserID)
		require.ErrorIs(t, err, db.ErrNotFound)
		loser, err := nodeRepo.GetNodeIncludingDeleted(ctx, loserID)
		require.NoError(t, err)
		require.NotNil(t, loser.DeletedAt)
		require.NotNil(t, loser.MergedInto)
		assert.Equal(t, winnerID, *loser.MergedInto)
	})

	t.Run("entity create + resolve + detail merge-patch", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteEntityTypesByKeyPrefix(ctx, gen.Prefix()) })

		// entity.subtype FKs entity_type.key, so seed a namespaced subtype first
		// (PR2 seeds the curated catalog; PR1 tests provide their own).
		subtype := gen.Prefix() + "tag"
		require.NoError(t, entityRepo.UpsertEntityType(ctx, repository.UpsertEntityTypeRequest{
			Key:    subtype,
			Status: repository.EntityTypeStatusProvisional,
		}))

		spec := gen.Entity(subtype)
		id := uuid.New()
		_, err := nodeRepo.CreateNode(ctx, id, spec.Node.Type, spec.Node.CanonicalLabel)
		require.NoError(t, err)

		ent, err := entityRepo.CreateEntity(ctx, repository.CreateEntityRequest{
			NodeID:         id,
			Subtype:        spec.Subtype,
			NormalizedName: spec.NormalizedName,
			Detail:         []byte(`{"color":"#abc"}`),
		})
		require.NoError(t, err)
		assert.Equal(t, id, ent.NodeID)
		assert.JSONEq(t, `{"color":"#abc"}`, string(ent.Detail))

		// Resolve via the (subtype, normalized_name) dedup lookup.
		resolved, err := entityRepo.FindEntityBySubtypeName(ctx, spec.Subtype, spec.NormalizedName)
		require.NoError(t, err)
		assert.Equal(t, id, resolved.NodeID)

		// Merge-patch adds a key without clobbering the existing color.
		require.NoError(t, entityRepo.UpdateEntityDetail(ctx, id, []byte(`{"icon":"star"}`)))
		patched, err := entityRepo.GetEntity(ctx, id)
		require.NoError(t, err)
		assert.JSONEq(t, `{"color":"#abc","icon":"star"}`, string(patched.Detail))
	})

	t.Run("entity create defaults nil detail to empty object", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteEntityTypesByKeyPrefix(ctx, gen.Prefix()) })

		subtype := gen.Prefix() + "topic"
		require.NoError(t, entityRepo.UpsertEntityType(ctx, repository.UpsertEntityTypeRequest{
			Key:    subtype,
			Status: repository.EntityTypeStatusProvisional,
		}))

		spec := gen.Entity(subtype)
		id := uuid.New()
		_, err := nodeRepo.CreateNode(ctx, id, spec.Node.Type, spec.Node.CanonicalLabel)
		require.NoError(t, err)

		// Nil detail must persist as the '{}' default, not SQL NULL.
		ent, err := entityRepo.CreateEntity(ctx, repository.CreateEntityRequest{
			NodeID:         id,
			Subtype:        spec.Subtype,
			NormalizedName: spec.NormalizedName,
			Detail:         nil,
		})
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(ent.Detail))
	})

	t.Run("entity (subtype, normalized_name) unique rejects a duplicate", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteEntityTypesByKeyPrefix(ctx, gen.Prefix()) })

		subtype := gen.Prefix() + "place"
		require.NoError(t, entityRepo.UpsertEntityType(ctx, repository.UpsertEntityTypeRequest{
			Key:    subtype,
			Status: repository.EntityTypeStatusProvisional,
		}))

		spec := gen.Entity(subtype)
		idA, idB := uuid.New(), uuid.New()
		_, err := nodeRepo.CreateNode(ctx, idA, spec.Node.Type, spec.Node.CanonicalLabel)
		require.NoError(t, err)
		_, err = nodeRepo.CreateNode(ctx, idB, "entity", gen.Prefix()+"place-dup")
		require.NoError(t, err)

		_, err = entityRepo.CreateEntity(ctx, repository.CreateEntityRequest{
			NodeID:         idA,
			Subtype:        spec.Subtype,
			NormalizedName: spec.NormalizedName,
		})
		require.NoError(t, err)

		// A second entity with the SAME (subtype, normalized_name) must fail.
		_, err = entityRepo.CreateEntity(ctx, repository.CreateEntityRequest{
			NodeID:         idB,
			Subtype:        spec.Subtype,
			NormalizedName: spec.NormalizedName,
		})
		require.Error(t, err)
	})

	t.Run("venue create + FindVenueByContainer + UpsertVenue idempotency", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		spec := gen.Venue("email", repository.VenueKindEmailThread)
		id := uuid.New()
		_, err := nodeRepo.CreateNode(ctx, id, spec.Node.Type, spec.Node.CanonicalLabel)
		require.NoError(t, err)

		title := spec.Title
		ven, err := venueRepo.CreateVenue(ctx, repository.CreateVenueRequest{
			NodeID:            id,
			Kind:              spec.Kind,
			Source:            spec.Source,
			SourceContainerID: spec.SourceContainerID,
			Title:             &title,
		})
		require.NoError(t, err)
		assert.Equal(t, id, ven.NodeID)

		// FindVenueByContainer resolves the same venue via the container unique.
		found, err := venueRepo.FindVenueByContainer(ctx, spec.Source, spec.Kind, spec.SourceContainerID)
		require.NoError(t, err)
		assert.Equal(t, id, found.NodeID)

		// UpsertVenue for the SAME container is idempotent: the INSERT loses the
		// ON CONFLICT race against the existing row, refreshes the title, and
		// returns the EXISTING node id. The spare node passed as the would-be
		// node_id is NOT consumed (no venue row is created for it) — which is
		// exactly why the live recorder helper (a later PR) must read-first before
		// minting a node, rather than create a node then UpsertVenue.
		newTitle := title + "-updated"
		spareNodeID := uuid.New()
		_, err = nodeRepo.CreateNode(ctx, spareNodeID, "venue", gen.Prefix()+"venue-upsert-spare")
		require.NoError(t, err)
		up, err := venueRepo.UpsertVenue(ctx, repository.CreateVenueRequest{
			NodeID:            spareNodeID,
			Kind:              spec.Kind,
			Source:            spec.Source,
			SourceContainerID: spec.SourceContainerID,
			Title:             &newTitle,
		})
		require.NoError(t, err)
		assert.Equal(t, id, up.NodeID, "upsert must return the existing venue node, not the spare")
		require.NotNil(t, up.Title)
		assert.Equal(t, newTitle, *up.Title)

		// The spare node has no venue row of its own (the upsert did not create a
		// second venue) — it is a bare node until namespace cleanup removes it.
		_, err = venueRepo.GetVenue(ctx, spareNodeID)
		require.ErrorIs(t, err, db.ErrNotFound, "spare node must not have gained a venue row")
	})

	t.Run("entity/venue live reads exclude a soft-deleted parent node", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })
		t.Cleanup(func() { _, _ = support.DeleteEntityTypesByKeyPrefix(ctx, gen.Prefix()) })

		// Entity/venue rows have no deleted_at of their own; liveness flows from
		// the parent node's tombstone, so the live reads must drop them once the
		// node is soft-deleted.
		subtype := gen.Prefix() + "org"
		require.NoError(t, entityRepo.UpsertEntityType(ctx, repository.UpsertEntityTypeRequest{
			Key:    subtype,
			Status: repository.EntityTypeStatusProvisional,
		}))

		entSpec := gen.Entity(subtype)
		entID := uuid.New()
		_, err := nodeRepo.CreateNode(ctx, entID, entSpec.Node.Type, entSpec.Node.CanonicalLabel)
		require.NoError(t, err)
		_, err = entityRepo.CreateEntity(ctx, repository.CreateEntityRequest{
			NodeID:         entID,
			Subtype:        entSpec.Subtype,
			NormalizedName: entSpec.NormalizedName,
		})
		require.NoError(t, err)

		venSpec := gen.Venue("gcal", repository.VenueKindMeeting)
		venID := uuid.New()
		_, err = nodeRepo.CreateNode(ctx, venID, venSpec.Node.Type, venSpec.Node.CanonicalLabel)
		require.NoError(t, err)
		_, err = venueRepo.CreateVenue(ctx, repository.CreateVenueRequest{
			NodeID:            venID,
			Kind:              venSpec.Kind,
			Source:            venSpec.Source,
			SourceContainerID: venSpec.SourceContainerID,
		})
		require.NoError(t, err)

		// Both resolve while their nodes are live.
		_, err = entityRepo.GetEntity(ctx, entID)
		require.NoError(t, err)
		_, err = entityRepo.FindEntityBySubtypeName(ctx, entSpec.Subtype, entSpec.NormalizedName)
		require.NoError(t, err)
		_, err = venueRepo.GetVenue(ctx, venID)
		require.NoError(t, err)
		_, err = venueRepo.FindVenueByContainer(ctx, venSpec.Source, venSpec.Kind, venSpec.SourceContainerID)
		require.NoError(t, err)

		// Soft-delete the parent nodes; the live reads must now miss them.
		require.NoError(t, nodeRepo.SoftDeleteNode(ctx, entID))
		require.NoError(t, nodeRepo.SoftDeleteNode(ctx, venID))

		_, err = entityRepo.GetEntity(ctx, entID)
		require.ErrorIs(t, err, db.ErrNotFound)
		_, err = entityRepo.FindEntityBySubtypeName(ctx, entSpec.Subtype, entSpec.NormalizedName)
		require.ErrorIs(t, err, db.ErrNotFound)
		_, err = venueRepo.GetVenue(ctx, venID)
		require.ErrorIs(t, err, db.ErrNotFound)
		_, err = venueRepo.FindVenueByContainer(ctx, venSpec.Source, venSpec.Kind, venSpec.SourceContainerID)
		require.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("venue (source, kind, source_container_id) unique rejects a duplicate", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		spec := gen.Venue("messages", repository.VenueKindGroupChat)
		idA, idB := uuid.New(), uuid.New()
		_, err := nodeRepo.CreateNode(ctx, idA, spec.Node.Type, spec.Node.CanonicalLabel)
		require.NoError(t, err)
		_, err = nodeRepo.CreateNode(ctx, idB, "venue", gen.Prefix()+"venue-dup")
		require.NoError(t, err)

		_, err = venueRepo.CreateVenue(ctx, repository.CreateVenueRequest{
			NodeID:            idA,
			Kind:              spec.Kind,
			Source:            spec.Source,
			SourceContainerID: spec.SourceContainerID,
		})
		require.NoError(t, err)

		_, err = venueRepo.CreateVenue(ctx, repository.CreateVenueRequest{
			NodeID:            idB,
			Kind:              spec.Kind,
			Source:            spec.Source,
			SourceContainerID: spec.SourceContainerID,
		})
		require.Error(t, err)
	})

	t.Run("entity_type upsert + get + namespace cleanup", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		key := gen.Prefix() + "topic"
		t.Cleanup(func() { _, _ = support.DeleteEntityTypesByKeyPrefix(ctx, gen.Prefix()) })

		require.NoError(t, entityRepo.UpsertEntityType(ctx, repository.UpsertEntityTypeRequest{
			Key:              key,
			Description:      "synthetic topic",
			ResolutionConfig: []byte(`{"k":1}`),
			Status:           repository.EntityTypeStatusProvisional,
		}))
		got, err := entityRepo.GetEntityType(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, repository.EntityTypeStatusProvisional, got.Status)
		assert.JSONEq(t, `{"k":1}`, string(got.ResolutionConfig))

		// Upsert is idempotent and updates on conflict.
		require.NoError(t, entityRepo.UpsertEntityType(ctx, repository.UpsertEntityTypeRequest{
			Key:         key,
			Description: "updated",
			Status:      repository.EntityTypeStatusCurated,
		}))
		got, err = entityRepo.GetEntityType(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, repository.EntityTypeStatusCurated, got.Status)
		assert.Equal(t, "updated", got.Description)
	})

	t.Run("CountNodesByLabelPrefix is namespace-scoped", func(t *testing.T) {
		t.Parallel()
		gen, _ := migrationGenerator(t)
		t.Cleanup(func() { _, _ = support.DeleteNodesByLabelPrefix(ctx, gen.Prefix()) })

		before, err := support.CountNodesByLabelPrefix(ctx, gen.Prefix())
		require.NoError(t, err)
		require.Equal(t, int64(0), before, "fresh namespace has no nodes")

		for i := 0; i < 3; i++ {
			spec := gen.Node(repository.NodeTypeEntity)
			_, err := nodeRepo.CreateNode(ctx, uuid.New(), spec.Type, spec.CanonicalLabel)
			require.NoError(t, err)
		}
		after, err := support.CountNodesByLabelPrefix(ctx, gen.Prefix())
		require.NoError(t, err)
		assert.Equal(t, int64(3), after)
	})
}
