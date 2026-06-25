package repository

import (
	"context"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
)

// Tag is the legacy user-defined tag row. The tag→graph migration mirrors each
// one into a `tag` entity node (color carried in the entity's detail JSONB);
// these reads exist only to drive that one-time `crm-admin --migrate-tags` run.
type Tag struct {
	ID    uuid.UUID
	Name  string
	Color *string
}

// ContactTagLink is one legacy contact_tag edge: which contact carries which
// tag, and when the link was created. CreatedAt is nil when the legacy row's
// created_at is NULL (the column is nullable: DEFAULT NOW() but no NOT NULL), so
// the migration can fall back to "now" rather than stamping a bogus zero time.
type ContactTagLink struct {
	ContactID uuid.UUID
	TagID     uuid.UUID
	CreatedAt *time.Time
}

// TagRepository reads the legacy tag / contact_tag tables for the tag→graph
// migration. There is no live tag UI, so these are the only consumers of the
// legacy tables until they are dropped.
type TagRepository struct {
	queries db.Querier
}

// NewTagRepository creates a new TagRepository.
func NewTagRepository(queries db.Querier) *TagRepository {
	return &TagRepository{queries: queries}
}

// ListTags returns every legacy tag, ordered by name.
func (r *TagRepository) ListTags(ctx context.Context) ([]Tag, error) {
	dbTags, err := r.queries.ListTags(ctx)
	if err != nil {
		return nil, err
	}
	tags := make([]Tag, 0, len(dbTags))
	for _, t := range dbTags {
		tag := Tag{
			ID:   uuid.UUID(t.ID.Bytes),
			Name: t.Name,
		}
		if t.Color.Valid {
			color := t.Color.String
			tag.Color = &color
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

// ListContactTagsWithLiveContact returns every contact_tag whose contact is NOT
// soft-deleted — the migration source set. Deleted contacts are skipped
// permanently (the assertion write path rejects a tombstoned subject node).
func (r *TagRepository) ListContactTagsWithLiveContact(ctx context.Context) ([]ContactTagLink, error) {
	dbLinks, err := r.queries.ListContactTagsWithLiveContact(ctx)
	if err != nil {
		return nil, err
	}
	links := make([]ContactTagLink, 0, len(dbLinks))
	for _, l := range dbLinks {
		link := ContactTagLink{
			ContactID: uuid.UUID(l.ContactID.Bytes),
			TagID:     uuid.UUID(l.TagID.Bytes),
		}
		// created_at is nullable; only carry it when present (an invalid
		// pgtype.Timestamptz would otherwise read as Go zero time).
		if l.CreatedAt.Valid {
			t := l.CreatedAt.Time.UTC()
			link.CreatedAt = &t
		}
		links = append(links, link)
	}
	return links, nil
}
