package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Node type constants. The node registry is the uniform anchor every graph
// entity (person, entity subtype, venue) is addressable through.
const (
	NodeTypePerson = "person"
	NodeTypeVenue  = "venue"
	NodeTypeEntity = "entity"
)

// Node is the thin, uniform registry row every graph entity attaches to. For a
// person, ID == the owning contact's id.
type Node struct {
	ID             uuid.UUID  `json:"id"`
	Type           string     `json:"type"`
	CanonicalLabel string     `json:"canonical_label"`
	CreatedAt      time.Time  `json:"created_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
	MergedInto     *uuid.UUID `json:"merged_into,omitempty"`
}

// NodeRepository handles node registry persistence.
type NodeRepository struct {
	queries db.Querier
}

// NewNodeRepository creates a new NodeRepository.
func NewNodeRepository(queries db.Querier) *NodeRepository {
	return &NodeRepository{queries: queries}
}

func convertDbNode(dbNode *db.Node) Node {
	node := Node{
		Type:           dbNode.Type,
		CanonicalLabel: dbNode.CanonicalLabel,
	}
	node.ID = dbNode.ID
	node.CreatedAt = dbNode.CreatedAt.UTC()
	node.DeletedAt = utcPtr(dbNode.DeletedAt)
	node.MergedInto = dbNode.MergedInto
	return node
}

// CreateNode inserts a node. The caller supplies id (for persons, id ==
// contact.id); node has no default id.
func (r *NodeRepository) CreateNode(ctx context.Context, id uuid.UUID, nodeType, canonicalLabel string) (*Node, error) {
	return createNode(ctx, r.queries, id, nodeType, canonicalLabel)
}

// CreateNodeTx is the tx-bound variant of CreateNode. Used by the contact→node
// dual-write so the node insert commits atomically with the contact row.
func (r *NodeRepository) CreateNodeTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, nodeType, canonicalLabel string) (*Node, error) {
	return createNode(ctx, db.New(tx), id, nodeType, canonicalLabel)
}

func createNode(ctx context.Context, q db.Querier, id uuid.UUID, nodeType, canonicalLabel string) (*Node, error) {
	dbNode, err := q.CreateNode(ctx, db.CreateNodeParams{
		ID:             id,
		Type:           nodeType,
		CanonicalLabel: canonicalLabel,
	})
	if err != nil {
		return nil, err
	}
	node := convertDbNode(dbNode)
	return &node, nil
}

// GetNode retrieves a live (non-soft-deleted) node by id.
func (r *NodeRepository) GetNode(ctx context.Context, id uuid.UUID) (*Node, error) {
	return getNode(ctx, r.queries, id)
}

// GetNodeTx is the tx-bound variant of GetNode. The write API reads the subject/
// object node inside its tx so the existence + soft-delete check sees in-tx state
// (e.g. a latent person node created earlier in the same tx).
func (r *NodeRepository) GetNodeTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Node, error) {
	return getNode(ctx, db.New(tx), id)
}

func getNode(ctx context.Context, q db.Querier, id uuid.UUID) (*Node, error) {
	dbNode, err := q.GetNode(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	node := convertDbNode(dbNode)
	return &node, nil
}

// GetNodeIncludingDeleted retrieves a node by id regardless of soft-delete
// state (e.g. to resolve a merged-away node).
func (r *NodeRepository) GetNodeIncludingDeleted(ctx context.Context, id uuid.UUID) (*Node, error) {
	dbNode, err := r.queries.GetNodeIncludingDeleted(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	node := convertDbNode(dbNode)
	return &node, nil
}

// SoftDeleteNode soft-deletes a node (sets deleted_at).
func (r *NodeRepository) SoftDeleteNode(ctx context.Context, id uuid.UUID) error {
	return r.queries.SoftDeleteNode(ctx, id)
}

// SoftDeleteNodeTx is the tx-bound variant of SoftDeleteNode.
func (r *NodeRepository) SoftDeleteNodeTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	return db.New(tx).SoftDeleteNode(ctx, id)
}

// SetNodeMergedInto records the merge alias (loser → winner) and tombstones the
// loser node in one statement.
func (r *NodeRepository) SetNodeMergedInto(ctx context.Context, loserID, winnerID uuid.UUID) error {
	return r.queries.SetNodeMergedInto(ctx, db.SetNodeMergedIntoParams{
		ID:         loserID,
		MergedInto: &winnerID,
	})
}

// SetNodeMergedIntoTx is the tx-bound variant of SetNodeMergedInto.
func (r *NodeRepository) SetNodeMergedIntoTx(ctx context.Context, tx pgx.Tx, loserID, winnerID uuid.UUID) error {
	return db.New(tx).SetNodeMergedInto(ctx, db.SetNodeMergedIntoParams{
		ID:         loserID,
		MergedInto: &winnerID,
	})
}

// TestCountVenueNodes counts venue-type nodes. Test-only.
func (r *NodeRepository) TestCountVenueNodes(ctx context.Context) (int64, error) {
	return r.queries.TestCountVenueNodes(ctx)
}

// TestCountOrphanVenueNodes counts venue-type nodes no live interaction
// references via venue_id. Test-only.
func (r *NodeRepository) TestCountOrphanVenueNodes(ctx context.Context) (int64, error) {
	return r.queries.TestCountOrphanVenueNodes(ctx)
}

// UpdateNodeCanonicalLabel keeps the node's display label loosely synced with
// its owning entity (e.g. a contact rename). The underlying query is :exec, so a
// missing node is a silent no-op (no rows-affected check). This is deliberate:
// the contact→node dual-write callers (CreateContact, UpdateContact,
// MergeContacts, enrichment) always pass a contact id whose person node exists
// — created in the same lifecycle by the dual-write and the 068 backfill — so 0
// rows cannot occur on those paths; and for any other caller a label sync
// against a non-existent node is harmlessly idempotent rather than an error.
func (r *NodeRepository) UpdateNodeCanonicalLabel(ctx context.Context, id uuid.UUID, canonicalLabel string) error {
	return r.queries.UpdateNodeCanonicalLabel(ctx, db.UpdateNodeCanonicalLabelParams{
		ID:             id,
		CanonicalLabel: canonicalLabel,
	})
}

// UpdateNodeCanonicalLabelTx is the tx-bound variant of
// UpdateNodeCanonicalLabel.
func (r *NodeRepository) UpdateNodeCanonicalLabelTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, canonicalLabel string) error {
	return db.New(tx).UpdateNodeCanonicalLabel(ctx, db.UpdateNodeCanonicalLabelParams{
		ID:             id,
		CanonicalLabel: canonicalLabel,
	})
}
