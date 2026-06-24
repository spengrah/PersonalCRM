package repository

import (
	"testing"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pure unit coverage of the graph-identity converters: they translate generated
// db.* rows (pgtype-wrapped) into the domain structs, handling NULL/invalid
// fields. DB-backed round-trips live in the integration suite.

func TestConvertDbNode(t *testing.T) {
	id := uuid.New()
	merged := uuid.New()
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	deleted := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	t.Run("live node, no merge", func(t *testing.T) {
		got := convertDbNode(&db.Node{
			ID:             pgtype.UUID{Bytes: id, Valid: true},
			Type:           NodeTypePerson,
			CanonicalLabel: "Person A",
			CreatedAt:      pgtype.Timestamptz{Time: created, Valid: true},
		})
		assert.Equal(t, id, got.ID)
		assert.Equal(t, NodeTypePerson, got.Type)
		assert.Equal(t, "Person A", got.CanonicalLabel)
		assert.Equal(t, created, got.CreatedAt)
		assert.Nil(t, got.DeletedAt)
		assert.Nil(t, got.MergedInto)
	})

	t.Run("merged + soft-deleted node", func(t *testing.T) {
		got := convertDbNode(&db.Node{
			ID:             pgtype.UUID{Bytes: id, Valid: true},
			Type:           NodeTypeEntity,
			CanonicalLabel: "Entity B",
			CreatedAt:      pgtype.Timestamptz{Time: created, Valid: true},
			DeletedAt:      pgtype.Timestamptz{Time: deleted, Valid: true},
			MergedInto:     pgtype.UUID{Bytes: merged, Valid: true},
		})
		require.NotNil(t, got.DeletedAt)
		assert.Equal(t, deleted, *got.DeletedAt)
		require.NotNil(t, got.MergedInto)
		assert.Equal(t, merged, *got.MergedInto)
	})
}

func TestConvertDbEntity(t *testing.T) {
	id := uuid.New()
	ref := "ext-123"

	t.Run("with external_ref + detail", func(t *testing.T) {
		got := convertDbEntity(&db.Entity{
			NodeID:         pgtype.UUID{Bytes: id, Valid: true},
			Subtype:        EntitySubtypeTag,
			NormalizedName: "friend",
			ExternalRef:    pgtype.Text{String: ref, Valid: true},
			Detail:         []byte(`{"color":"#fff"}`),
		})
		assert.Equal(t, id, got.NodeID)
		assert.Equal(t, EntitySubtypeTag, got.Subtype)
		assert.Equal(t, "friend", got.NormalizedName)
		require.NotNil(t, got.ExternalRef)
		assert.Equal(t, ref, *got.ExternalRef)
		assert.JSONEq(t, `{"color":"#fff"}`, string(got.Detail))
	})

	t.Run("null external_ref", func(t *testing.T) {
		got := convertDbEntity(&db.Entity{
			NodeID:         pgtype.UUID{Bytes: id, Valid: true},
			Subtype:        EntitySubtypePlace,
			NormalizedName: "nyc",
			ExternalRef:    pgtype.Text{Valid: false},
			Detail:         []byte(`{}`),
		})
		assert.Nil(t, got.ExternalRef)
	})
}

func TestConvertDbVenue(t *testing.T) {
	id := uuid.New()

	t.Run("with title", func(t *testing.T) {
		got := convertDbVenue(&db.Venue{
			NodeID:            pgtype.UUID{Bytes: id, Valid: true},
			Kind:              VenueKindEmailThread,
			Source:            "email",
			SourceContainerID: "thread-1",
			Title:             pgtype.Text{String: "Subject", Valid: true},
		})
		assert.Equal(t, id, got.NodeID)
		assert.Equal(t, VenueKindEmailThread, got.Kind)
		assert.Equal(t, "email", got.Source)
		assert.Equal(t, "thread-1", got.SourceContainerID)
		require.NotNil(t, got.Title)
		assert.Equal(t, "Subject", *got.Title)
	})

	t.Run("null title", func(t *testing.T) {
		got := convertDbVenue(&db.Venue{
			NodeID:            pgtype.UUID{Bytes: id, Valid: true},
			Kind:              VenueKindCall,
			Source:            "phone_calls",
			SourceContainerID: "call-1",
			Title:             pgtype.Text{Valid: false},
		})
		assert.Nil(t, got.Title)
	})
}

func TestConvertDbEntityType(t *testing.T) {
	got := convertDbEntityType(&db.EntityType{
		Key:              EntitySubtypePlace,
		Description:      "places",
		ResolutionConfig: []byte(`{"hierarchical":true}`),
		Status:           EntityTypeStatusCurated,
	})
	assert.Equal(t, EntitySubtypePlace, got.Key)
	assert.Equal(t, "places", got.Description)
	assert.JSONEq(t, `{"hierarchical":true}`, string(got.ResolutionConfig))
	assert.Equal(t, EntityTypeStatusCurated, got.Status)
}
