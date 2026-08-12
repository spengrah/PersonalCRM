package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// Embedding target_kind constants. The kind names the source table the
// target_id points at (polymorphic, no FK — embeddings are disposable
// projections). The closed set matches the embedding table's CHECK enum.
const (
	EmbeddingTargetNode            = "node"
	EmbeddingTargetAssertion       = "assertion"
	EmbeddingTargetInteraction     = "interaction"
	EmbeddingTargetMeetingNote     = "meeting_note"
	EmbeddingTargetCommsMessage    = "comms_message"
	EmbeddingTargetTelegramMessage = "telegram_message"
	EmbeddingTargetMessagesMessage = "messages_message"
)

// Embedding is a stored vector(1536) for one (target_kind, target_id,
// model_version). A disposable projection: no envelope, no deleted_at.
type Embedding struct {
	TargetKind   string    `json:"target_kind"`
	TargetID     uuid.UUID `json:"target_id"`
	ModelVersion string    `json:"model_version"`
	Vector       []float32 `json:"vector"`
	ComputedAt   time.Time `json:"computed_at"`
}

// UpsertEmbeddingRequest is the input for storing an embedding.
type UpsertEmbeddingRequest struct {
	TargetKind   string
	TargetID     uuid.UUID
	ModelVersion string
	Vector       []float32
}

// EmbeddingRepository handles embedding persistence.
type EmbeddingRepository struct {
	queries db.Querier
}

// NewEmbeddingRepository creates a new EmbeddingRepository.
func NewEmbeddingRepository(queries db.Querier) *EmbeddingRepository {
	return &EmbeddingRepository{queries: queries}
}

func convertDbEmbedding(dbEmbedding *db.Embedding) Embedding {
	embedding := Embedding{
		TargetKind:   dbEmbedding.TargetKind,
		ModelVersion: dbEmbedding.ModelVersion,
		Vector:       dbEmbedding.Vector.Slice(),
	}
	embedding.TargetID = dbEmbedding.TargetID
	embedding.ComputedAt = dbEmbedding.ComputedAt.UTC()
	return embedding
}

// UpsertEmbedding idempotently stores the embedding for one
// (target_kind, target_id, model_version); a recompute overwrites the vector.
func (r *EmbeddingRepository) UpsertEmbedding(ctx context.Context, req UpsertEmbeddingRequest) error {
	return r.queries.UpsertEmbedding(ctx, db.UpsertEmbeddingParams{
		TargetKind:   req.TargetKind,
		TargetID:     req.TargetID,
		ModelVersion: req.ModelVersion,
		Vector:       pgvector.NewVector(req.Vector),
	})
}

// GetEmbedding retrieves one embedding by its composite key.
func (r *EmbeddingRepository) GetEmbedding(ctx context.Context, targetKind string, targetID uuid.UUID, modelVersion string) (*Embedding, error) {
	dbEmbedding, err := r.queries.GetEmbedding(ctx, db.GetEmbeddingParams{
		TargetKind:   targetKind,
		TargetID:     targetID,
		ModelVersion: modelVersion,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	embedding := convertDbEmbedding(dbEmbedding)
	return &embedding, nil
}

// DeleteEmbeddingsForTarget wipes every model's embedding for one target.
func (r *EmbeddingRepository) DeleteEmbeddingsForTarget(ctx context.Context, targetKind string, targetID uuid.UUID) error {
	return r.queries.DeleteEmbeddingsForTarget(ctx, db.DeleteEmbeddingsForTargetParams{
		TargetKind: targetKind,
		TargetID:   targetID,
	})
}
