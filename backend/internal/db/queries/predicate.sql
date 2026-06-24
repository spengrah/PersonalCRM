-- Predicate catalog queries (graph foundation).
--
-- The reads deliberately project every column EXCEPT embedding. The embedding is
-- a nullable vector(1536) populated/consumed in a later layer; the pgvector-go
-- value type cannot scan a SQL NULL (it panics decoding an empty buffer), so
-- this layer — which never needs the embedding — simply does not select it.

-- name: GetPredicate :one
SELECT
    key, kind, subject_type, object_type, value_type, cardinality, "symmetric",
    inverse_predicate, temporal_profile, base_rate_days, typical_duration_days,
    default_salience, default_review_policy, proposition_bucket, status,
    description, synonyms, created_at
FROM predicate WHERE key = $1;

-- name: ListPredicatesByStatus :many
SELECT
    key, kind, subject_type, object_type, value_type, cardinality, "symmetric",
    inverse_predicate, temporal_profile, base_rate_days, typical_duration_days,
    default_salience, default_review_policy, proposition_bucket, status,
    description, synonyms, created_at
FROM predicate WHERE status = $1 ORDER BY key;

-- name: ListCuratedPredicates :many
SELECT
    key, kind, subject_type, object_type, value_type, cardinality, "symmetric",
    inverse_predicate, temporal_profile, base_rate_days, typical_duration_days,
    default_salience, default_review_policy, proposition_bucket, status,
    description, synonyms, created_at
FROM predicate WHERE status = 'curated' ORDER BY key;

-- name: CreatePredicate :one
-- Mints a (typically provisional) predicate at runtime. The embedding column is
-- omitted from the insert so it falls to its NULL default — provisional minting
-- never carries an embedding (those are populated separately) — and from the
-- RETURNING projection so the NULL vector is never scanned back.
INSERT INTO predicate (
    key, kind, subject_type, object_type, value_type, cardinality, "symmetric",
    inverse_predicate, temporal_profile, base_rate_days, typical_duration_days,
    default_salience, default_review_policy, proposition_bucket, status,
    description, synonyms
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
)
RETURNING
    key, kind, subject_type, object_type, value_type, cardinality, "symmetric",
    inverse_predicate, temporal_profile, base_rate_days, typical_duration_days,
    default_salience, default_review_policy, proposition_bucket, status,
    description, synonyms, created_at;
