-- Event queries (spec §3.1, §3.3). Raw append-only event log.

-- name: InsertEvent :one
-- Insert an event; conflicts on (source, source_id) (when source_id is not
-- NULL) return zero rows so the caller can treat as idempotent no-op. When
-- sqlc.narg('id') is NULL, the DB generates a fresh UUID via
-- gen_random_uuid().
INSERT INTO event (id, source, source_id, kind, payload, observed_at)
VALUES (
    COALESCE(sqlc.narg('id')::uuid, gen_random_uuid()),
    @source,
    sqlc.narg('source_id')::text,
    @kind,
    @payload,
    @observed_at
)
ON CONFLICT (source, source_id) WHERE source_id IS NOT NULL DO NOTHING
RETURNING *;

-- name: GetEvent :one
SELECT * FROM event WHERE id = @id;

-- name: FindEventBySource :one
-- Primary use: publisher-side dedup lookup BEFORE attempting insert (e.g.,
-- batch ingestion can pre-filter duplicates). Also used by tests.
--
-- source_id is nullable at the table level, but this lookup is only
-- meaningful for non-null source_ids (NULL source_ids are not deduped —
-- they always insert, see the partial unique index in migration 036).
-- The repository layer rejects empty sourceID with db.ErrNotFound before
-- calling this query.
SELECT * FROM event
WHERE source = @source AND source_id = @source_id
LIMIT 1;

-- name: CountEventsBySource :one
-- Used by integration tests (and potentially ops dashboards) to assert
-- ingest outcomes per source. The event log is append-only, so no
-- deleted_at filter is needed.
SELECT COUNT(*) FROM event WHERE source = @source;
