package repository

import (
	"context"
	"errors"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Venue kind constants. A venue node is a shared interaction container.
const (
	VenueKindEmailThread = "email_thread"
	VenueKindGroupChat   = "group_chat"
	VenueKindDM          = "dm"
	VenueKindMeeting     = "meeting"
	VenueKindCall        = "call"
	VenueKindSession     = "session"
)

// Venue is the structural subtype row for a shared-container node. NodeID is
// both PK and the FK alias of node.id.
type Venue struct {
	NodeID            uuid.UUID `json:"node_id"`
	Kind              string    `json:"kind"`
	Source            string    `json:"source"`
	SourceContainerID string    `json:"source_container_id"`
	Title             *string   `json:"title,omitempty"`
}

// CreateVenueRequest is the input for creating a venue subtype row.
type CreateVenueRequest struct {
	NodeID            uuid.UUID
	Kind              string
	Source            string
	SourceContainerID string
	Title             *string
}

// VenueRepository handles venue subtype persistence.
type VenueRepository struct {
	queries db.Querier
}

// NewVenueRepository creates a new VenueRepository.
func NewVenueRepository(queries db.Querier) *VenueRepository {
	return &VenueRepository{queries: queries}
}

func convertDbVenue(dbVenue *db.Venue) Venue {
	venue := Venue{
		Kind:              dbVenue.Kind,
		Source:            dbVenue.Source,
		SourceContainerID: dbVenue.SourceContainerID,
	}
	if dbVenue.NodeID.Valid {
		venue.NodeID = uuid.UUID(dbVenue.NodeID.Bytes)
	}
	if dbVenue.Title.Valid {
		venue.Title = &dbVenue.Title.String
	}
	return venue
}

// CreateVenue inserts a venue subtype row.
func (r *VenueRepository) CreateVenue(ctx context.Context, req CreateVenueRequest) (*Venue, error) {
	return createVenue(ctx, r.queries, req)
}

// CreateVenueTx is the tx-bound variant of CreateVenue.
func (r *VenueRepository) CreateVenueTx(ctx context.Context, tx pgx.Tx, req CreateVenueRequest) (*Venue, error) {
	return createVenue(ctx, db.New(tx), req)
}

func createVenue(ctx context.Context, q db.Querier, req CreateVenueRequest) (*Venue, error) {
	dbVenue, err := q.CreateVenue(ctx, db.CreateVenueParams{
		NodeID:            uuidToPgUUID(req.NodeID),
		Kind:              req.Kind,
		Source:            req.Source,
		SourceContainerID: req.SourceContainerID,
		Title:             stringToPgText(req.Title),
	})
	if err != nil {
		return nil, err
	}
	venue := convertDbVenue(dbVenue)
	return &venue, nil
}

// GetVenue retrieves a venue by its node id.
func (r *VenueRepository) GetVenue(ctx context.Context, nodeID uuid.UUID) (*Venue, error) {
	dbVenue, err := r.queries.GetVenue(ctx, uuidToPgUUID(nodeID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	venue := convertDbVenue(dbVenue)
	return &venue, nil
}

// FindVenueByContainer resolves the single venue for a real container via the
// (source, kind, source_container_id) unique.
func (r *VenueRepository) FindVenueByContainer(ctx context.Context, source, kind, sourceContainerID string) (*Venue, error) {
	dbVenue, err := r.queries.FindVenueByContainer(ctx, db.FindVenueByContainerParams{
		Source:            source,
		Kind:              kind,
		SourceContainerID: sourceContainerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	venue := convertDbVenue(dbVenue)
	return &venue, nil
}

// UpsertVenue idempotently creates a venue for a container (refreshing the title
// on conflict). Used by the PR6 interaction backfill / live recorders so a
// re-run for the same container returns the existing row.
func (r *VenueRepository) UpsertVenue(ctx context.Context, req CreateVenueRequest) (*Venue, error) {
	return upsertVenue(ctx, r.queries, req)
}

// UpsertVenueTx is the tx-bound variant of UpsertVenue.
func (r *VenueRepository) UpsertVenueTx(ctx context.Context, tx pgx.Tx, req CreateVenueRequest) (*Venue, error) {
	return upsertVenue(ctx, db.New(tx), req)
}

func upsertVenue(ctx context.Context, q db.Querier, req CreateVenueRequest) (*Venue, error) {
	dbVenue, err := q.UpsertVenue(ctx, db.UpsertVenueParams{
		NodeID:            uuidToPgUUID(req.NodeID),
		Kind:              req.Kind,
		Source:            req.Source,
		SourceContainerID: req.SourceContainerID,
		Title:             stringToPgText(req.Title),
	})
	if err != nil {
		return nil, err
	}
	venue := convertDbVenue(dbVenue)
	return &venue, nil
}
