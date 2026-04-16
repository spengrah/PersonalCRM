-- Event shadow cadence-observation queries (spec §3.4.2, PR 7). Sibling
-- table to event_shadow_observation (migration 038); captures cadence-
-- column pre/post image per event so the post-bake divergence query can
-- FULL OUTER JOIN on event_id. See
-- .ai/log/plan/event-bus-foundation-pr7-cadence-updater-shadow.md.

-- name: InsertCadenceShadowObservation :one
-- Inserts an observation row. The UNIQUE (event_id, writer) constraint
-- (migration 039) gives at-most-one-row-per-writer semantics under river
-- retries. ON CONFLICT DO NOTHING returns no rows on duplicate; callers
-- treat "no rows" as idempotent success rather than an error.
INSERT INTO event_shadow_cadence_observation (
    event_id, writer, contact_id, source, direction, branch, occurred_at,
    prev_last_contacted, prev_last_outreach_at, prev_last_response_at, prev_contact_by,
    next_last_contacted, next_last_outreach_at, next_last_response_at, next_contact_by,
    apply_last_contacted, apply_last_outreach_at, apply_last_response_at, apply_contact_by
) VALUES (
    @event_id,
    @writer,
    @contact_id,
    @source,
    @direction,
    @branch,
    @occurred_at,
    sqlc.narg('prev_last_contacted')::timestamptz,
    sqlc.narg('prev_last_outreach_at')::timestamptz,
    sqlc.narg('prev_last_response_at')::timestamptz,
    sqlc.narg('prev_contact_by')::date,
    sqlc.narg('next_last_contacted')::timestamptz,
    sqlc.narg('next_last_outreach_at')::timestamptz,
    sqlc.narg('next_last_response_at')::timestamptz,
    sqlc.narg('next_contact_by')::date,
    @apply_last_contacted,
    @apply_last_outreach_at,
    @apply_last_response_at,
    @apply_contact_by
)
ON CONFLICT (event_id, writer) DO NOTHING
RETURNING *;

-- name: FindCadenceShadowObservationByEventAndWriter :one
-- Inline divergence logger uses this: given a consumer row just inserted,
-- look up the paired 'direct' row to compare next_* values. Returns
-- sql.ErrNoRows when the direct path hasn't observed yet (expected when
-- the consumer fires before the direct-path post-commit closure lands).
SELECT * FROM event_shadow_cadence_observation
WHERE event_id = @event_id AND writer = @writer
LIMIT 1;

-- name: CountCadenceShadowObservationsByWriter :one
-- Used by integration tests and bake-window evidence collection.
SELECT COUNT(*) FROM event_shadow_cadence_observation
WHERE writer = @writer;

-- name: FindCadenceShadowDivergences :many
-- Post-bake divergence query. FULL OUTER JOIN of direct vs consumer rows
-- on event_id — per plan Decision 1 each (event_id, writer) pair is unique,
-- so the join resolves at most one direct + one consumer row per event.
-- A non-empty result indicates real drift: direct missing, consumer
-- missing, or next_* values disagreeing.
--
-- Race-class filters baked in (plan Decision 4 taxonomy):
--
--   1. `observed_at < @observed_at_to - 5s` — skip rows observed within
--      the grace window. The direct-path observer fires from an async
--      post-commit closure that may land after the consumer; a rigid
--      boundary would flag rows as "direct missing" that will land
--      moments later.
--   2. Exclude events where the paired contact is soft-deleted
--      (contact.deleted_at IS NOT NULL). The direct-path closure skips
--      snapshot writes when the contact is gone (plan Decision 4
--      contact_soft_deleted_midstream race class); the consumer may
--      still have written because its check runs at event-time, not
--      query-time. These rows are expected-one-sided.
-- @observed_at_to::timestamptz - INTERVAL '5 seconds' is the effective
-- upper bound; the grace window lets the direct-path post-commit
-- closure land before we treat "direct missing" as a real divergence.
-- Keep @observed_at_to as the "hard" right edge so callers can reason
-- about the bake window; pre-subtract the grace inline.
WITH direct_obs AS (
    SELECT * FROM event_shadow_cadence_observation
    WHERE writer = 'direct'
      AND observed_at >= sqlc.arg(observed_at_from)::timestamptz
      AND observed_at <  sqlc.arg(observed_at_to)::timestamptz - INTERVAL '5 seconds'
),
consumer_obs AS (
    SELECT * FROM event_shadow_cadence_observation
    WHERE writer = 'consumer'
      AND observed_at >= sqlc.arg(observed_at_from)::timestamptz
      AND observed_at <  sqlc.arg(observed_at_to)::timestamptz - INTERVAL '5 seconds'
),
joined AS (
    SELECT
        COALESCE(d.event_id, c.event_id)       AS event_id,
        COALESCE(d.contact_id, c.contact_id)   AS contact_id,
        d.branch                               AS direct_branch,
        c.branch                               AS consumer_branch,
        d.next_last_contacted                  AS direct_next_last_contacted,
        c.next_last_contacted                  AS consumer_next_last_contacted,
        d.next_last_outreach_at                AS direct_next_last_outreach_at,
        c.next_last_outreach_at                AS consumer_next_last_outreach_at,
        d.next_last_response_at                AS direct_next_last_response_at,
        c.next_last_response_at                AS consumer_next_last_response_at,
        d.next_contact_by                      AS direct_next_contact_by,
        c.next_contact_by                      AS consumer_next_contact_by
    FROM direct_obs d
    FULL OUTER JOIN consumer_obs c USING (event_id)
    WHERE d.event_id IS NULL
       OR c.event_id IS NULL
       OR d.branch                IS DISTINCT FROM c.branch
       OR d.next_last_contacted   IS DISTINCT FROM c.next_last_contacted
       OR d.next_last_outreach_at IS DISTINCT FROM c.next_last_outreach_at
       OR d.next_last_response_at IS DISTINCT FROM c.next_last_response_at
       OR d.next_contact_by       IS DISTINCT FROM c.next_contact_by
)
SELECT j.*
FROM joined j
LEFT JOIN contact ct ON ct.id = j.contact_id
WHERE ct.deleted_at IS NULL;
