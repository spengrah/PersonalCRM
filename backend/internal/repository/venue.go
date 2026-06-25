package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"

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

// venueNodeNamespace is the fixed UUID namespace the venue node id is derived
// from. The venue node id is uuid_generate_v5(namespace, <length-prefixed
// source|kind|container>), computed identically here (uuid.NewSHA1 == UUID v5)
// and in the 069 migration backfill so the live recorders and the backfill
// converge on ONE venue node per real container. Changing this value would
// orphan every existing venue node — it is frozen.
var venueNodeNamespace = uuid.MustParse("a4f7c0e2-1b3d-4c6a-9e8f-2d5b7a1c0e94")

// venueContainerName builds the length-prefixed (source, kind, container)
// encoding used as both the uuid_generate_v5 name and the advisory-lock key.
// Length-prefixing each component (<len>:<value>) means a delimiter or NUL byte
// inside a container id cannot forge a collision across components. MUST stay
// byte-identical to the migration's encoding.
func venueContainerName(source, kind, container string) string {
	return lengthPrefix(source) + "|" + lengthPrefix(kind) + "|" + lengthPrefix(container)
}

func lengthPrefix(s string) string {
	return strconv.Itoa(len(s)) + ":" + s
}

// VenueNodeID returns the deterministic venue node id for a container. Exported
// so tests can assert the live helper and the migration agree.
func VenueNodeID(source, kind, container string) uuid.UUID {
	return uuid.NewSHA1(venueNodeNamespace, []byte(venueContainerName(source, kind, container)))
}

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
// on conflict). Used by the interaction backfill / live recorders so a re-run
// for the same container returns the existing row.
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

// ResolveVenueForInteraction returns the venue node id for a real container,
// creating the node+venue pair on first sight. Called by the live interaction
// recorders inside the recording tx so the venue_id is set atomically with the
// interaction insert.
//
// No-orphan-node creation under concurrency: the venue node id is deterministic
// (VenueNodeID), so the node + venue inserts are both ON CONFLICT DO NOTHING and
// a concurrent same-container writer can never leak an orphan node. The helper
// still reads first (fast path, no lock) and, only on a miss, takes a
// per-container advisory lock before the create — so two recorders that race on
// a brand-new container serialize on creation and exactly one pair is written.
// The deterministic id keeps this in lockstep with the 069 migration backfill.
func (r *VenueRepository) ResolveVenueForInteraction(
	ctx context.Context, tx pgx.Tx, source, kind, containerKey, title string,
) (uuid.UUID, error) {
	q := db.New(tx)

	// Fast path: the venue already exists for this container.
	if existing, err := q.FindVenueByContainer(ctx, db.FindVenueByContainerParams{
		Source:            source,
		Kind:              kind,
		SourceContainerID: containerKey,
	}); err == nil {
		return uuid.UUID(existing.NodeID.Bytes), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("find venue by container: %w", err)
	}

	// Miss: serialize concurrent creation of THIS container on an advisory lock
	// so exactly one node+venue pair is created (the lock auto-releases on
	// commit/rollback).
	if err := q.AcquireVenueContainerLock(ctx, venueContainerName(source, kind, containerKey)); err != nil {
		return uuid.Nil, fmt.Errorf("acquire venue container lock: %w", err)
	}

	// Re-check under the lock: a racing writer may have created it between our
	// read and the lock.
	if existing, err := q.FindVenueByContainer(ctx, db.FindVenueByContainerParams{
		Source:            source,
		Kind:              kind,
		SourceContainerID: containerKey,
	}); err == nil {
		return uuid.UUID(existing.NodeID.Bytes), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("re-check venue by container: %w", err)
	}

	// Still absent: create the node+venue pair with the deterministic id. Both
	// inserts are ON CONFLICT DO NOTHING; the deterministic id is what makes the
	// node insert safe (no orphan) even if a writer slipped in.
	nodeID := VenueNodeID(source, kind, containerKey)
	if _, err := q.CreateVenueNode(ctx, db.CreateVenueNodeParams{
		NodeID:            uuidToPgUUID(nodeID),
		Kind:              kind,
		Source:            source,
		SourceContainerID: containerKey,
		Title:             stringToPgText(nilIfEmpty(title)),
		// Node canonical_label is kept empty for venue nodes (the human label
		// lives on venue.title); this matches the 069 migration backfill so a
		// venue created live and one created by the backfill are identical.
		CanonicalLabel: "",
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("create venue node: %w", err)
	}
	return nodeID, nil
}

// nilIfEmpty maps an empty string to a nil *string (SQL NULL title) and a
// non-empty string to its pointer.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
