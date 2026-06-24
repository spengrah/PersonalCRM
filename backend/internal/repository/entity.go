package repository

import (
	"context"
	"errors"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Entity subtype constants (the curated-core entity_type keys; the catalog rows
// are seeded by the predicate-catalog migration). Provisional subtypes are
// minted at runtime.
const (
	EntitySubtypeOrganization = "organization"
	EntitySubtypePlace        = "place"
	EntitySubtypeTopic        = "topic"
	EntitySubtypeTag          = "tag"
)

// Entity-type status constants.
const (
	EntityTypeStatusCurated     = "curated"
	EntityTypeStatusProvisional = "provisional"
)

// Entity is the structural subtype row for a non-person, non-venue node. NodeID
// is both PK and the FK alias of node.id. Detail is the per-instance attribute
// bag (raw JSONB); nil/empty round-trips as the DB default '{}'.
type Entity struct {
	NodeID         uuid.UUID `json:"node_id"`
	Subtype        string    `json:"subtype"`
	NormalizedName string    `json:"normalized_name"`
	ExternalRef    *string   `json:"external_ref,omitempty"`
	Detail         []byte    `json:"detail"`
}

// EntityType is a row in the per-TYPE entity-subtype catalog. ResolutionConfig
// is raw JSONB consumed by later entity resolution.
type EntityType struct {
	Key              string `json:"key"`
	Description      string `json:"description"`
	ResolutionConfig []byte `json:"resolution_config"`
	Status           string `json:"status"`
}

// UpsertEntityTypeRequest is the input for seeding an entity-type catalog row.
// ResolutionConfig may be nil (defaults to '{}' at the DB).
type UpsertEntityTypeRequest struct {
	Key              string
	Description      string
	ResolutionConfig []byte
	Status           string
}

// CreateEntityRequest is the input for creating an entity subtype row. Detail
// may be nil (defaults to '{}' at the DB).
type CreateEntityRequest struct {
	NodeID         uuid.UUID
	Subtype        string
	NormalizedName string
	ExternalRef    *string
	Detail         []byte
}

// EntityRepository handles entity subtype persistence.
type EntityRepository struct {
	queries db.Querier
}

// NewEntityRepository creates a new EntityRepository.
func NewEntityRepository(queries db.Querier) *EntityRepository {
	return &EntityRepository{queries: queries}
}

func convertDbEntity(dbEntity *db.Entity) Entity {
	entity := Entity{
		Subtype:        dbEntity.Subtype,
		NormalizedName: dbEntity.NormalizedName,
		Detail:         dbEntity.Detail,
	}
	if dbEntity.NodeID.Valid {
		entity.NodeID = uuid.UUID(dbEntity.NodeID.Bytes)
	}
	if dbEntity.ExternalRef.Valid {
		entity.ExternalRef = &dbEntity.ExternalRef.String
	}
	return entity
}

// CreateEntity inserts an entity subtype row.
func (r *EntityRepository) CreateEntity(ctx context.Context, req CreateEntityRequest) (*Entity, error) {
	return createEntity(ctx, r.queries, req)
}

// CreateEntityTx is the tx-bound variant of CreateEntity.
func (r *EntityRepository) CreateEntityTx(ctx context.Context, tx pgx.Tx, req CreateEntityRequest) (*Entity, error) {
	return createEntity(ctx, db.New(tx), req)
}

func createEntity(ctx context.Context, q db.Querier, req CreateEntityRequest) (*Entity, error) {
	dbEntity, err := q.CreateEntity(ctx, db.CreateEntityParams{
		NodeID:         uuidToPgUUID(req.NodeID),
		Subtype:        req.Subtype,
		NormalizedName: req.NormalizedName,
		ExternalRef:    stringToPgText(req.ExternalRef),
		Detail:         jsonbOrEmpty(req.Detail),
	})
	if err != nil {
		return nil, err
	}
	entity := convertDbEntity(dbEntity)
	return &entity, nil
}

// GetEntity retrieves an entity by its node id.
func (r *EntityRepository) GetEntity(ctx context.Context, nodeID uuid.UUID) (*Entity, error) {
	return getEntity(ctx, r.queries, nodeID)
}

// GetEntityTx is the tx-bound variant of GetEntity. The write API resolves an
// entity-subtype subject's subtype inside its tx so subject-type validation (e.g.
// `within` place→place) sees in-tx state.
func (r *EntityRepository) GetEntityTx(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID) (*Entity, error) {
	return getEntity(ctx, db.New(tx), nodeID)
}

func getEntity(ctx context.Context, q db.Querier, nodeID uuid.UUID) (*Entity, error) {
	dbEntity, err := q.GetEntity(ctx, uuidToPgUUID(nodeID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	entity := convertDbEntity(dbEntity)
	return &entity, nil
}

// FindEntityBySubtypeName resolves an entity via the (subtype, normalized_name)
// unique — the entity-resolution dedup lookup.
func (r *EntityRepository) FindEntityBySubtypeName(ctx context.Context, subtype, normalizedName string) (*Entity, error) {
	dbEntity, err := r.queries.FindEntityBySubtypeName(ctx, db.FindEntityBySubtypeNameParams{
		Subtype:        subtype,
		NormalizedName: normalizedName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	entity := convertDbEntity(dbEntity)
	return &entity, nil
}

// UpdateEntityDetail merge-patches the per-instance detail JSONB (existing keys
// not in the patch are preserved via the || operator).
func (r *EntityRepository) UpdateEntityDetail(ctx context.Context, nodeID uuid.UUID, patch []byte) error {
	return r.queries.UpdateEntityDetail(ctx, db.UpdateEntityDetailParams{
		NodeID: uuidToPgUUID(nodeID),
		// A nil patch would make `detail || NULL` evaluate to NULL (violating the
		// NOT NULL); '{}' makes it a no-op merge that preserves existing keys.
		Detail: jsonbOrEmpty(patch),
	})
}

func convertDbEntityType(dbEntityType *db.EntityType) EntityType {
	return EntityType{
		Key:              dbEntityType.Key,
		Description:      dbEntityType.Description,
		ResolutionConfig: dbEntityType.ResolutionConfig,
		Status:           dbEntityType.Status,
	}
}

// UpsertEntityType idempotently seeds an entity-type catalog row (the curated
// subtypes are seeded by the predicate-catalog migration).
func (r *EntityRepository) UpsertEntityType(ctx context.Context, req UpsertEntityTypeRequest) error {
	return r.queries.UpsertEntityType(ctx, db.UpsertEntityTypeParams{
		Key:              req.Key,
		Description:      req.Description,
		ResolutionConfig: jsonbOrEmpty(req.ResolutionConfig),
		Status:           req.Status,
	})
}

// GetEntityType retrieves an entity-type catalog row by key.
func (r *EntityRepository) GetEntityType(ctx context.Context, key string) (*EntityType, error) {
	dbEntityType, err := r.queries.GetEntityType(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	entityType := convertDbEntityType(dbEntityType)
	return &entityType, nil
}
