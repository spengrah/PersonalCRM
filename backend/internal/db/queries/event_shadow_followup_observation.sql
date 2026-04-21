-- Event shadow follow-up observation queries (spec §3.4.3). Sibling to
-- event_shadow_observation (migration 038) and
-- event_shadow_cadence_observation (migration 039). Captures the action
-- the FollowUpManager DID (direct path) or WOULD do (consumer) per
-- interaction.recorded event.

-- name: InsertFollowUpShadowObservation :one
-- Inserts an observation row. The UNIQUE (event_id, writer) constraint
-- (migration 042) gives at-most-one-row-per-writer semantics under river
-- retries. ON CONFLICT DO NOTHING returns no rows on duplicate; callers
-- treat "no rows" as idempotent success rather than an error.
INSERT INTO event_shadow_followup_observation (
    event_id, writer, contact_id, source, direction, occurred_at,
    action, skip_reason, would_idempotency_key, would_deadline,
    direct_contact_task_id, consumer_called_todoist
) VALUES (
    @event_id,
    @writer,
    @contact_id,
    @source,
    @direction,
    @occurred_at,
    @action,
    sqlc.narg('skip_reason')::text,
    sqlc.narg('would_idempotency_key')::text,
    sqlc.narg('would_deadline')::date,
    sqlc.narg('direct_contact_task_id')::uuid,
    @consumer_called_todoist
)
ON CONFLICT (event_id, writer) DO NOTHING
RETURNING *;

-- name: FindFollowUpShadowObservationByEventAndWriter :one
-- Used by the inline divergence logger and by integration tests that
-- need to read back a specific writer's observation.
SELECT * FROM event_shadow_followup_observation
WHERE event_id = @event_id AND writer = @writer
LIMIT 1;

-- name: CountFollowUpShadowObservationsByWriter :one
SELECT COUNT(*) FROM event_shadow_followup_observation
WHERE writer = @writer;

-- name: CountFollowUpShadowObservationsByContact :one
-- Per-contact count, narrower than CountByWriter. Integration tests use
-- this to assert invariants scoped to the contacts they own and avoid
-- racing against concurrent writes on the shared test DB.
SELECT COUNT(*) FROM event_shadow_followup_observation
WHERE contact_id = @contact_id;

-- name: FindFollowUpShadowDivergences :many
-- Post-bake divergence query. FULL OUTER JOIN of direct vs consumer
-- rows on event_id — UNIQUE (event_id, writer) guarantees at most one
-- row per side per event, so the join produces at most one direct +
-- one consumer row per event. A non-empty result indicates real drift
-- once expected-divergence classes (guard 1 backdated, guard 2
-- out-of-order) are filtered out at the report layer.
--
-- Race-class filters baked in (mirrors PR 7's cadence-shadow query):
--
--   1. Grace window applied on the joined pair, NOT each side
--      independently. The direct-path observer fires from an async
--      post-commit closure that may land after the consumer. Filtering
--      each CTE alone causes a false consumer-only divergence when
--      consumer lands past the grace edge and direct lands inside it.
--   2. Exclude pairs where the contact is soft-deleted. The direct
--      path skips when the contact is gone; the consumer may still
--      have written because its check runs at event-time.
WITH direct_obs AS (
    SELECT * FROM event_shadow_followup_observation
    WHERE writer = 'direct'
      AND observed_at >= sqlc.arg(observed_at_from)::timestamptz
      AND observed_at <  sqlc.arg(observed_at_to)::timestamptz
),
consumer_obs AS (
    SELECT * FROM event_shadow_followup_observation
    WHERE writer = 'consumer'
      AND observed_at >= sqlc.arg(observed_at_from)::timestamptz
      AND observed_at <  sqlc.arg(observed_at_to)::timestamptz
),
joined AS (
    SELECT
        COALESCE(d.event_id, c.event_id)       AS event_id,
        COALESCE(d.contact_id, c.contact_id)   AS contact_id,
        GREATEST(d.observed_at, c.observed_at) AS pair_observed_at,
        d.action                                AS direct_action,
        c.action                                AS consumer_action,
        d.skip_reason                           AS direct_skip_reason,
        c.skip_reason                           AS consumer_skip_reason,
        d.would_idempotency_key                 AS direct_would_idempotency_key,
        c.would_idempotency_key                 AS consumer_would_idempotency_key,
        d.would_deadline                        AS direct_would_deadline,
        c.would_deadline                        AS consumer_would_deadline,
        d.direct_contact_task_id                AS direct_contact_task_id,
        c.consumer_called_todoist               AS consumer_called_todoist
    FROM direct_obs d
    FULL OUTER JOIN consumer_obs c USING (event_id)
    WHERE (d.event_id IS NULL
       OR c.event_id IS NULL
       OR d.action            IS DISTINCT FROM c.action
       OR d.skip_reason       IS DISTINCT FROM c.skip_reason
       OR d.would_deadline    IS DISTINCT FROM c.would_deadline)
      AND GREATEST(d.observed_at, c.observed_at)
          < sqlc.arg(observed_at_to)::timestamptz - INTERVAL '5 seconds'
)
SELECT
    j.event_id,
    j.contact_id,
    j.direct_action,
    j.consumer_action,
    j.direct_skip_reason,
    j.consumer_skip_reason,
    j.direct_would_idempotency_key,
    j.consumer_would_idempotency_key,
    j.direct_would_deadline,
    j.consumer_would_deadline,
    j.direct_contact_task_id,
    j.consumer_called_todoist
FROM joined j
LEFT JOIN contact ct ON ct.id = j.contact_id
WHERE ct.deleted_at IS NULL;
