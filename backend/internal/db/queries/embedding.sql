-- Embedding storage queries (graph foundation, derived storage).
--
-- The embedding table is a disposable projection: a vector(1536) keyed by a
-- polymorphic (target_kind, target_id) + model_version. Storage only — SP1 has
-- no generators, so there is no per-kind read fan-out or vector-search query
-- here (that arrives in SP3 with the index).

-- name: UpsertEmbedding :exec
-- Idempotently store the embedding for one (target_kind, target_id,
-- model_version): a recompute for the same key overwrites the vector and
-- refreshes computed_at.
INSERT INTO embedding (target_kind, target_id, model_version, vector, computed_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (target_kind, target_id, model_version)
DO UPDATE SET vector = EXCLUDED.vector, computed_at = EXCLUDED.computed_at;

-- name: GetEmbedding :one
SELECT * FROM embedding
WHERE target_kind = $1 AND target_id = $2 AND model_version = $3;

-- name: DeleteEmbeddingsForTarget :exec
-- Wipe every model's embedding for one target (e.g. when the target's content
-- changes and all embeddings must be rebuilt).
DELETE FROM embedding
WHERE target_kind = $1 AND target_id = $2;
