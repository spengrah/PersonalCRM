-- Event shadow observation queries (spec §3.8, PR 5). Append-only log of
-- what each write path (direct vs consumer) produced during shadow-mode
-- bake. The post-bake divergence query FULL OUTER JOINs direct+consumer
-- rows on (source, source_ref) to surface drift.

-- name: InsertEventShadowObservation :one
-- Inserts an observation row and returns it. No ON CONFLICT — each call
-- produces a new row. The event_id FK may be NULL for direct-path rows
-- that fired without a paired event envelope (e.g., ExtendInteraction,
-- PromoteInteractionToMutual).
INSERT INTO event_shadow_observation (
    event_id, writer, kind, source, source_ref, contact_id,
    direction, occurred_at, interaction_id, replay
) VALUES (
    sqlc.narg('event_id')::uuid,
    @writer,
    @kind,
    @source,
    sqlc.narg('source_ref')::text,
    @contact_id,
    @direction,
    @occurred_at,
    sqlc.narg('interaction_id')::uuid,
    @replay
)
RETURNING *;

-- name: CountShadowObservationsByWriter :one
-- Used by integration tests to assert exact row counts per writer. No
-- deleted_at filter — append-only table.
SELECT COUNT(*) FROM event_shadow_observation
WHERE writer = @writer;

-- name: FindMatchingDirectWriteBySourceRef :one
-- Fetches the most recent direct-path row matching the given
-- (source, source_ref, contact_id). Used by the inline divergence logger
-- (Decision 14 Part A) when the consumer commits a writer='consumer' row
-- and wants to compare against the peer direct-path row. Returns the most
-- recent fresh-write (kind = 'direct_record') row when multiple exist.
SELECT * FROM event_shadow_observation
WHERE writer = 'direct'
  AND kind = 'direct_record'
  AND source = @source
  AND source_ref = @source_ref
  AND contact_id = @contact_id
ORDER BY observed_at DESC
LIMIT 1;

-- name: FindMatchingDirectWriteByManual :one
-- Manual (no source_ref) variant: matches on (source, contact_id) and the
-- occurred_at truncated to the second. The 30-minute dedup window of the
-- direct path guarantees a single row per contact per minute-level ts.
SELECT * FROM event_shadow_observation
WHERE writer = 'direct'
  AND kind = 'direct_record'
  AND source = @source
  AND contact_id = @contact_id
  AND source_ref IS NULL
  AND date_trunc('second', occurred_at) = date_trunc('second', @occurred_at::timestamptz)
ORDER BY observed_at DESC
LIMIT 1;

-- name: FindShadowDivergencesRefBearing :many
-- Post-bake divergence query for ref-bearing kinds. FULL OUTER JOIN of
-- direct-fresh-write rows vs consumer-fresh-write rows on
-- (source, source_ref, contact_id). A non-empty result means one or more
-- interactions disagreed between the two paths (direction mismatch,
-- occurred_at mismatch, or one-side-only). Parameters bound the observation
-- time range so the bake-window evidence is reproducible.
WITH direct_writes AS (
    SELECT source, source_ref, contact_id, direction,
           date_trunc('second', occurred_at)::timestamptz AS ts
    FROM event_shadow_observation
    WHERE writer = 'direct'
      AND replay = false
      AND kind = 'direct_record'
      AND source_ref IS NOT NULL
      AND observed_at >= @observed_at_from::timestamptz
      AND observed_at < @observed_at_to::timestamptz
),
consumer_writes AS (
    SELECT source, source_ref, contact_id, direction,
           date_trunc('second', occurred_at)::timestamptz AS ts
    FROM event_shadow_observation
    WHERE writer = 'consumer'
      AND replay = false
      AND source_ref IS NOT NULL
      AND observed_at >= @observed_at_from::timestamptz
      AND observed_at < @observed_at_to::timestamptz
)
SELECT
    COALESCE(d.source, c.source)         AS source,
    COALESCE(d.source_ref, c.source_ref) AS source_ref,
    COALESCE(d.contact_id, c.contact_id) AS contact_id,
    d.direction                          AS direct_direction,
    c.direction                          AS consumer_direction,
    d.ts                                 AS direct_ts,
    c.ts                                 AS consumer_ts
FROM direct_writes d
FULL OUTER JOIN consumer_writes c
    USING (source, source_ref, contact_id)
WHERE d.direction IS DISTINCT FROM c.direction
   OR d.ts IS DISTINCT FROM c.ts;

-- name: FindShadowDivergencesManual :many
-- Post-bake divergence query for manual (no-source_ref) kind. Joins on
-- (source, contact_id, ts-second). Same semantics as the ref-bearing
-- variant.
WITH direct_writes AS (
    SELECT source, contact_id, direction,
           date_trunc('second', occurred_at)::timestamptz AS ts
    FROM event_shadow_observation
    WHERE writer = 'direct'
      AND replay = false
      AND kind = 'direct_record'
      AND source_ref IS NULL
      AND observed_at >= @observed_at_from::timestamptz
      AND observed_at < @observed_at_to::timestamptz
),
consumer_writes AS (
    SELECT source, contact_id, direction,
           date_trunc('second', occurred_at)::timestamptz AS ts
    FROM event_shadow_observation
    WHERE writer = 'consumer'
      AND replay = false
      AND source_ref IS NULL
      AND observed_at >= @observed_at_from::timestamptz
      AND observed_at < @observed_at_to::timestamptz
)
SELECT
    COALESCE(d.source, c.source)         AS source,
    COALESCE(d.contact_id, c.contact_id) AS contact_id,
    d.direction                          AS direct_direction,
    c.direction                          AS consumer_direction,
    d.ts                                 AS direct_ts,
    c.ts                                 AS consumer_ts
FROM direct_writes d
FULL OUTER JOIN consumer_writes c
    USING (source, contact_id, ts)
WHERE d.direction IS DISTINCT FROM c.direction;
