package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TagMigrationConfidence is the confidence stamped on a migrated tag assertion.
// Tags are an explicit user action, so they land at full confidence.
const TagMigrationConfidence = 100

// ErrTagCaseCollision is returned when two legacy tags differ only by case
// (e.g. "Friend" and "friend"). The entity store is unique on
// (subtype, lower(name)), so collapsing them silently would merge two distinct
// user tags. The migration fails loudly and the operator dedups the legacy tags
// first, picking the canonical casing.
var ErrTagCaseCollision = errors.New("tag-migration: case-insensitive name collision")

// TagMigrationResult is the counts-only summary of a --migrate-tags run.
type TagMigrationResult struct {
	Tags                   int // legacy tag rows scanned
	TagNodesCreated        int // tag entity nodes newly created (re-runs reuse existing)
	TagNodesExisting       int // tag entity nodes already present (idempotent re-run)
	ContactTags            int // contact_tag rows of non-deleted contacts processed (migrated)
	SkippedDeletedContacts int // contact_tag rows skipped because their contact is soft-deleted
	AssertionsAsserted     int // tagged_as asserts issued (deduped downstream by AssertService)
}

// TagMigrationService mirrors the legacy tag / contact_tag tables into the graph:
// each tag becomes a `tag` entity node (color carried in the entity's detail
// JSONB) and each contact_tag of a non-deleted contact becomes an accepted
// `tagged_as` assertion authored by the user. It routes every assertion through
// AssertService so proposition identity + dedup + event emission apply, which
// makes the whole run idempotent (re-running corroborates rather than
// duplicates).
type TagMigrationService struct {
	pool       *pgxpool.Pool
	tagRepo    *repository.TagRepository
	nodeRepo   *repository.NodeRepository
	entityRepo *repository.EntityRepository
	assertSvc  *AssertService
}

// NewTagMigrationService wires the migration over the legacy tag reads + the
// graph node/entity repos + the validated assertion write path.
func NewTagMigrationService(
	pool *pgxpool.Pool,
	tagRepo *repository.TagRepository,
	nodeRepo *repository.NodeRepository,
	entityRepo *repository.EntityRepository,
	assertSvc *AssertService,
) *TagMigrationService {
	return &TagMigrationService{
		pool:       pool,
		tagRepo:    tagRepo,
		nodeRepo:   nodeRepo,
		entityRepo: entityRepo,
		assertSvc:  assertSvc,
	}
}

// MigrateTags runs the full tag→graph migration. It is idempotent: a re-run
// reuses existing tag entity nodes (find-or-create by (subtype, lower(name)))
// and re-asserts each tagged_as edge, which AssertService corroborates rather
// than duplicates.
func (s *TagMigrationService) MigrateTags(ctx context.Context) (TagMigrationResult, error) {
	var result TagMigrationResult

	tags, err := s.tagRepo.ListTags(ctx)
	if err != nil {
		return result, fmt.Errorf("list tags: %w", err)
	}
	result.Tags = len(tags)

	// Case-collision preflight: detect legacy tags that differ only by case
	// BEFORE creating any node, so the run is all-or-nothing on collision.
	if err := detectTagCaseCollisions(tags); err != nil {
		return result, err
	}

	// Find-or-create one tag entity node per lower(name), recording the node id
	// so the contact_tag pass can resolve each tag's object node.
	tagNodeByTagID := make(map[uuid.UUID]uuid.UUID, len(tags))
	for _, tag := range tags {
		nodeID, created, err := s.findOrCreateTagNode(ctx, tag)
		if err != nil {
			return result, fmt.Errorf("find-or-create tag node %q: %w", tag.Name, err)
		}
		tagNodeByTagID[tag.ID] = nodeID
		if created {
			result.TagNodesCreated++
		} else {
			result.TagNodesExisting++
		}
	}

	// Count the contact_tags skipped because their contact is soft-deleted, so the
	// summary reports the skip explicitly (the migrated count below covers only
	// non-deleted contacts, which won't match the raw contact_tag table otherwise).
	skipped, err := s.tagRepo.CountContactTagsWithDeletedContact(ctx)
	if err != nil {
		return result, fmt.Errorf("count skipped contact_tags: %w", err)
	}
	result.SkippedDeletedContacts = int(skipped)

	// Mirror each contact_tag of a non-deleted contact into a tagged_as assertion.
	links, err := s.tagRepo.ListContactTagsWithLiveContact(ctx)
	if err != nil {
		return result, fmt.Errorf("list contact_tags: %w", err)
	}
	result.ContactTags = len(links)
	for _, link := range links {
		tagNodeID, ok := tagNodeByTagID[link.TagID]
		if !ok {
			// A contact_tag referencing a tag not in the tag table is a data
			// inconsistency (the tag_id FK is ON DELETE CASCADE, so this should be
			// impossible). Fail loudly rather than silently skip.
			return result, fmt.Errorf("contact_tag references unknown tag %s", link.TagID)
		}
		if err := s.assertTaggedAs(ctx, link, tagNodeID); err != nil {
			return result, fmt.Errorf("assert tagged_as (contact %s tag %s): %w", link.ContactID, link.TagID, err)
		}
		result.AssertionsAsserted++
	}

	return result, nil
}

// detectTagCaseCollisions groups tags by lower(name) and returns
// ErrTagCaseCollision (listing every colliding group's original names) if any
// group has more than one member. Names within a group are sorted for a stable
// error message.
func detectTagCaseCollisions(tags []repository.Tag) error {
	byLower := make(map[string][]string)
	for _, tag := range tags {
		key := strings.ToLower(strings.TrimSpace(tag.Name))
		byLower[key] = append(byLower[key], tag.Name)
	}
	var groups []string
	for _, names := range byLower {
		if len(names) > 1 {
			sort.Strings(names)
			groups = append(groups, fmt.Sprintf("{%s}", strings.Join(names, ", ")))
		}
	}
	if len(groups) == 0 {
		return nil
	}
	sort.Strings(groups)
	return fmt.Errorf("%w: dedup these legacy tags (pick one canonical casing per group) before re-running: %s",
		ErrTagCaseCollision, strings.Join(groups, " "))
}

// findOrCreateTagNode resolves the tag entity node for a legacy tag, creating
// the node + entity row (with color in detail) when absent. Returns the node id
// and whether it was newly created. Find-first makes a re-run reuse the existing
// node (no duplicate entity nodes).
func (s *TagMigrationService) findOrCreateTagNode(ctx context.Context, tag repository.Tag) (uuid.UUID, bool, error) {
	normalizedName := strings.ToLower(strings.TrimSpace(tag.Name))

	// Find-first idempotency rests on a constraint mismatch worth flagging:
	// FindEntityBySubtypeName excludes a tag whose node is soft-deleted (it joins
	// node and filters node.deleted_at), but the entity `(subtype, normalized_name)`
	// unique index does NOT. There is no tag-node soft-delete path in the graph
	// today, so this is unreachable; if a future tag-delete feature ever tombstones
	// a tag node, re-running this migration would find nothing here and then hit a
	// 23505 on the re-create. That fails LOUDLY (no silent corruption), but a future
	// tag-delete path must resurrect the existing node (clear deleted_at) instead of
	// creating a second one.
	existing, err := s.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypeTag, normalizedName)
	if err == nil {
		return existing.NodeID, false, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return uuid.Nil, false, fmt.Errorf("find tag entity: %w", err)
	}

	detail, err := tagDetailJSON(tag.Color)
	if err != nil {
		return uuid.Nil, false, err
	}

	// Create the node + entity rows atomically: the entity.node_id FK requires the
	// node to exist, and both must commit together so a partial tag node never
	// lingers.
	nodeID := uuid.New()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := s.nodeRepo.CreateNodeTx(ctx, tx, nodeID, repository.NodeTypeEntity, tag.Name); err != nil {
		return uuid.Nil, false, fmt.Errorf("create tag node: %w", err)
	}
	if _, err := s.entityRepo.CreateEntityTx(ctx, tx, repository.CreateEntityRequest{
		NodeID:         nodeID,
		Subtype:        repository.EntitySubtypeTag,
		NormalizedName: normalizedName,
		Detail:         detail,
	}); err != nil {
		return uuid.Nil, false, fmt.Errorf("create tag entity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, false, fmt.Errorf("commit tag node: %w", err)
	}
	return nodeID, true, nil
}

// tagDetailJSON builds the entity detail JSONB carrying the tag's color. A nil
// color yields nil detail (the entity repo defaults it to '{}'), so the color
// has a single source of truth (the entity node) for both migrated and new tags.
func tagDetailJSON(color *string) ([]byte, error) {
	if color == nil || strings.TrimSpace(*color) == "" {
		return nil, nil
	}
	detail, err := json.Marshal(map[string]string{"color": *color})
	if err != nil {
		return nil, fmt.Errorf("marshal tag detail: %w", err)
	}
	return detail, nil
}

// assertTaggedAs issues the accepted tagged_as edge for one contact_tag link. The
// subject is the contact's person node (node.id == contact.id); the
// person-node backfill creates one for every non-deleted contact, so in normal
// operation it exists — but if it is missing, AssertService rejects the write
// loudly (subject-node validation) and the migration aborts rather than skipping
// silently. The object is the tag entity node. User provenance + the
// contact_tag's created_at as the knowledge time make this a faithful, idempotent
// mirror of the legacy link.
func (s *TagMigrationService) assertTaggedAs(ctx context.Context, link repository.ContactTagLink, tagNodeID uuid.UUID) error {
	// A deterministic source_id keys the provenance idempotently: a re-run hashes
	// to the same locator and ON CONFLICT no-ops rather than appending a duplicate.
	sourceID := fmt.Sprintf("tag-migration:%s:%s", link.ContactID, link.TagID)
	req := AssertRequest{
		SubjectNodeID: link.ContactID,
		PredicateKey:  "tagged_as",
		ObjectNodeID:  &tagNodeID,
		Confidence:    TagMigrationConfidence,
		Locators: []ProvenanceLocator{{
			SourceKind:   repository.SourceKindUser,
			SourceID:     sourceID,
			ProducerKind: repository.ProducerKindUser,
		}},
	}
	// Preserve the legacy link's created_at as the knowledge time when present; a
	// NULL legacy created_at has no knowable knowledge time, so leave the override
	// nil and let AssertService default knowledge_from to now (the honest fallback,
	// not a bogus zero time).
	if link.CreatedAt != nil {
		req.KnowledgeFromOverride = link.CreatedAt
	}
	_, err := s.assertSvc.Assert(ctx, req)
	return err
}
