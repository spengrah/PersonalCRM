-- Relationship signal storage queries (graph foundation, derived storage).
--
-- relationship_signal is a disposable projection: a per-node scalar keyed by
-- (subject_node_id, signal_key). subject_node_id is a real FK→node. Storage
-- only — SP1 has no generators, so there is no ranking/aggregation read here
-- (that arrives in SP3 when signals go live).

-- name: UpsertRelationshipSignal :exec
-- Idempotently store one (subject_node_id, signal_key) signal: a recompute for
-- the same key overwrites the value and refreshes the watermarks.
INSERT INTO relationship_signal
    (subject_node_id, signal_key, value, computed_at, as_of, method_version)
VALUES ($1, $2, $3, NOW(), $4, $5)
ON CONFLICT (subject_node_id, signal_key)
DO UPDATE SET
    value = EXCLUDED.value,
    computed_at = EXCLUDED.computed_at,
    as_of = EXCLUDED.as_of,
    method_version = EXCLUDED.method_version;

-- name: GetRelationshipSignal :one
SELECT * FROM relationship_signal
WHERE subject_node_id = $1 AND signal_key = $2;

-- name: ListSignalsForSubject :many
SELECT * FROM relationship_signal
WHERE subject_node_id = $1
ORDER BY signal_key;

-- name: DeleteSignalsForSubject :exec
-- Wipe every signal for one node (e.g. when the node's inputs change and all
-- signals must be rebuilt).
DELETE FROM relationship_signal
WHERE subject_node_id = $1;
