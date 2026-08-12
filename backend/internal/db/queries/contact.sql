-- Contact queries

-- name: GetContact :one
SELECT * FROM contact
WHERE id = $1 AND deleted_at IS NULL;

-- name: ContactIsLive :one
-- Liveness probe for the identity-match guard: a cached
-- external_identity.contact_id pointing at a soft-deleted (e.g. merged-away)
-- contact must not short-circuit the discovery path.
SELECT EXISTS(
    SELECT 1 FROM contact
    WHERE id = $1 AND deleted_at IS NULL
) AS is_live;

-- name: CreateContactWithNode :one
-- The person node and the contact are inserted by ONE statement, so the pair is
-- atomic at pool scope as well as inside a caller's transaction. The outer
-- INSERT selects new_node.id as the contact's id: data-modifying CTEs run in an
-- unpredictable order and RETURNING is the only channel between them, so the
-- read is what forces node-before-contact and what guarantees the two rows
-- share an id by construction rather than by passing the same argument twice.
-- location/birthday/how_met are NOT written here: they are derived cache
-- columns whose sole writer is the knowledge-cache consumer, which fills
-- them from the current-accepted lives_in/birthday/how_met assertions
-- ContactService emits in the same tx. The columns retain their values
-- via that consumer; the create INSERT leaves them at their DB default
-- (NULL) until the consumer refreshes.
WITH new_node AS (
    INSERT INTO node (id, type, canonical_label, created_at)
    VALUES (sqlc.arg(id), 'person', sqlc.arg(full_name), sqlc.arg(created_at))
    RETURNING id
)
INSERT INTO contact (id, full_name, cadence, last_contacted, profile_photo, created_at, contact_by)
SELECT new_node.id, sqlc.arg(full_name), sqlc.arg(cadence), sqlc.narg(last_contacted),
       sqlc.arg(profile_photo), sqlc.arg(created_at), sqlc.arg(contact_by)
FROM new_node
RETURNING contact.*;

-- name: UpdateContact :one
-- Profile-only update path. Writes name, cadence, profile_photo — NEVER
-- writes last_contacted, last_outreach_at, last_response_at, contact_by,
-- or the location/birthday/how_met cache columns (those flow from the
-- knowledge-cache consumer off the assertion store).
-- ContactService.UpdateContact handles the cadence-change side-effect
-- (recomputing contact_by) by calling
-- CadenceUpdater.ApplyContactByOverride in the same tx;
-- EnrichmentService uses this query for cadence-absent inferred fields
-- and CadenceUpdater.ApplyContactByOverride when the input DTO carries
-- an explicit cadence preference.
UPDATE contact SET
  full_name = $2,
  cadence = $3,
  profile_photo = $4,
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: UpdateContactLocationCache :exec
-- Knowledge-cache sole-writer: refreshes the derived location cache column
-- from the current-accepted lives_in edge's place node label (NULL when no
-- current value). updated_at is intentionally NOT bumped — a cache refresh
-- is bookkeeping, not a user profile edit.
UPDATE contact SET location = $2 WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateContactBirthdayCache :exec
-- Knowledge-cache sole-writer: refreshes the derived birthday cache column
-- from the current-accepted birthday fact (NULL when no current value).
UPDATE contact SET birthday = $2 WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateContactHowMetCache :exec
-- Knowledge-cache sole-writer: refreshes the derived how_met cache column
-- from the current-accepted how_met fact (NULL when no current value).
UPDATE contact SET how_met = $2 WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateContactCadenceForward :exec
-- Forward-only cadence write (spec §3.4.2). Each of the cadence columns
-- is updated only when its apply-flag is true AND the new value strictly
-- exceeds the existing one (or the existing is NULL).
--
-- last_interaction_at is gated by its OWN apply flag
-- (apply_last_interaction_at), independent of apply_last_contacted.
-- Interaction-driven paths (HandleEvent, ApplyInteraction) set both
-- flags together so inbound/mutual still bump last_interaction_at
-- alongside last_contacted, matching the pre-cutover
-- UpdateContactResponseFields/UpdateContactMutualFields write surface.
-- Merge (BulkApply) sets apply_last_interaction_at=false because a
-- merge is not an interaction and must not mutate the "last
-- non-outbound interaction" timestamp of the surviving contact.
UPDATE contact SET
    last_contacted = CASE
        WHEN sqlc.arg(apply_last_contacted)::boolean
          AND (last_contacted IS NULL OR sqlc.narg(last_contacted)::timestamptz > last_contacted)
        THEN sqlc.narg(last_contacted)::timestamptz
        ELSE last_contacted
    END,
    last_interaction_at = CASE
        WHEN sqlc.arg(apply_last_interaction_at)::boolean
          AND (last_interaction_at IS NULL OR sqlc.narg(last_interaction_at)::timestamptz > last_interaction_at)
        THEN sqlc.narg(last_interaction_at)::timestamptz
        ELSE last_interaction_at
    END,
    last_outreach_at = CASE
        WHEN sqlc.arg(apply_last_outreach_at)::boolean
          AND (last_outreach_at IS NULL OR sqlc.narg(last_outreach_at)::timestamptz > last_outreach_at)
        THEN sqlc.narg(last_outreach_at)::timestamptz
        ELSE last_outreach_at
    END,
    last_response_at = CASE
        WHEN sqlc.arg(apply_last_response_at)::boolean
          AND (last_response_at IS NULL OR sqlc.narg(last_response_at)::timestamptz > last_response_at)
        THEN sqlc.narg(last_response_at)::timestamptz
        ELSE last_response_at
    END,
    contact_by = CASE
        WHEN sqlc.arg(apply_contact_by)::boolean
          AND sqlc.narg('contact_by')::date IS NOT NULL
          AND (contact_by IS NULL OR sqlc.narg('contact_by')::date > contact_by)
        THEN sqlc.narg('contact_by')::date
        ELSE contact_by
    END,
    updated_at = NOW()
WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: UpdateContactCadenceUnconditional :exec
-- Manual-source branch (spec §3.4.2 "manual-source exception"): user
-- correction — any passed-in value replaces the existing one
-- unconditionally. Apply-flags still gate which columns are touched
-- (e.g., a manual outbound still shouldn't bump last_contacted per
-- direction rules).
--
-- last_interaction_at is gated by its OWN apply flag
-- (apply_last_interaction_at); see UpdateContactCadenceForward above
-- for the rationale.
UPDATE contact SET
    last_contacted = CASE
        WHEN sqlc.arg(apply_last_contacted)::boolean THEN sqlc.narg(last_contacted)::timestamptz
        ELSE last_contacted
    END,
    last_interaction_at = CASE
        WHEN sqlc.arg(apply_last_interaction_at)::boolean THEN sqlc.narg(last_interaction_at)::timestamptz
        ELSE last_interaction_at
    END,
    last_outreach_at = CASE
        WHEN sqlc.arg(apply_last_outreach_at)::boolean THEN sqlc.narg(last_outreach_at)::timestamptz
        ELSE last_outreach_at
    END,
    last_response_at = CASE
        WHEN sqlc.arg(apply_last_response_at)::boolean THEN sqlc.narg(last_response_at)::timestamptz
        ELSE last_response_at
    END,
    contact_by = CASE
        WHEN sqlc.arg(apply_contact_by)::boolean THEN sqlc.narg('contact_by')::date
        ELSE contact_by
    END,
    updated_at = NOW()
WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: SnapshotContactCadenceFields :one
-- Returns only the four spec-listed cadence columns. Used by PR 7's
-- direct-path post-commit closure to capture the post-image inside its
-- own short-lived tx (plan Decision 5). Consumer does NOT call this —
-- consumer reads prev from the event payload (plan Decision 2a).
SELECT last_contacted, last_outreach_at, last_response_at, contact_by
FROM contact
WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: SoftDeleteContact :exec
UPDATE contact SET
  deleted_at = NOW(),
  updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: FindSimilarContacts :many
SELECT
  c.id,
  c.full_name,
  similarity(c.full_name, sqlc.arg(search_name)::text) as name_similarity,
  COALESCE(
    json_agg(
      json_build_object(
        'type', cm.type,
        'value', cm.value
      )
    ) FILTER (WHERE cm.id IS NOT NULL),
    '[]'
  )::jsonb as methods_json
FROM contact c
LEFT JOIN contact_method cm ON c.id = cm.contact_id
WHERE c.deleted_at IS NULL
  AND similarity(c.full_name, sqlc.arg(search_name)::text) > sqlc.arg(threshold)::real
GROUP BY c.id, c.full_name
ORDER BY similarity(c.full_name, sqlc.arg(search_name)::text) DESC
LIMIT sqlc.arg(result_limit);

-- name: FindSimilarContactsBatch :many
-- Finds similar contacts for multiple candidate names in a single batch query.
-- Uses UNNEST to expand input arrays and LATERAL join to find matches per candidate.
-- Returns results grouped by candidate_id with matches ordered by similarity.
WITH candidate_names AS (
  SELECT
    unnest(sqlc.arg(candidate_names)::text[])::text as candidate_name,
    unnest(sqlc.arg(candidate_ids)::text[])::text as candidate_id
)
SELECT
  cn.candidate_id::text as candidate_id,
  cn.candidate_name::text as candidate_name,
  c.id as contact_id,
  c.full_name as contact_name,
  similarity(c.full_name, cn.candidate_name) as name_similarity,
  COALESCE(
    json_agg(
      json_build_object(
        'type', cm.type,
        'value', cm.value
      )
    ) FILTER (WHERE cm.id IS NOT NULL),
    '[]'
  )::jsonb as methods_json
FROM candidate_names cn
CROSS JOIN LATERAL (
  SELECT c.id, c.full_name
  FROM contact c
  WHERE c.deleted_at IS NULL
    AND similarity(c.full_name, cn.candidate_name) > sqlc.arg(threshold)::real
  ORDER BY similarity(c.full_name, cn.candidate_name) DESC
  LIMIT sqlc.arg(limit_per_candidate)
) c
LEFT JOIN contact_method cm ON c.id = cm.contact_id
GROUP BY cn.candidate_id, cn.candidate_name, c.id, c.full_name
ORDER BY cn.candidate_id, similarity(c.full_name, cn.candidate_name) DESC;

-- name: ListOverdueContacts :many
-- Lists contacts whose contact_by date is before today (overdue).
-- Returns contacts ordered by how overdue they are (most overdue first).
SELECT * FROM contact
WHERE deleted_at IS NULL
  AND contact_by IS NOT NULL
  AND contact_by < sqlc.arg(today)::date
ORDER BY contact_by ASC
LIMIT sqlc.arg(limit_count);

-- name: ListContactsWithContactBy :many
-- Lists contacts that have a contact_by date set (used for testing mode filtering).
-- Returns contacts ordered by contact_by (soonest first).
SELECT * FROM contact
WHERE deleted_at IS NULL
  AND contact_by IS NOT NULL
ORDER BY contact_by ASC
LIMIT $1;

-- name: ListContactsWithCadence :many
-- Lists contacts that have a cadence set (used for Todoist sync reconciliation).
SELECT * FROM contact
WHERE deleted_at IS NULL
  AND cadence IS NOT NULL
  AND cadence != ''
ORDER BY full_name ASC
LIMIT $1;

-- name: LockContactForDateRecompute :one
-- Acquires a FOR UPDATE lock on the contact row before the date recompute
-- reads the interaction aggregate. Run as a SEPARATE statement (in the
-- caller's tx) BEFORE ComputeContactDatesAfterDelete: once this lock is held
-- — waiting out any concurrent interaction/cadence writer, whose atomic
-- (interaction INSERT + contact UPDATE) tx takes a conflicting row lock —
-- the subsequent ComputeContactDatesAfterDelete statement runs at a fresh
-- READ COMMITTED snapshot that INCLUDES that writer's just-committed
-- interaction rows. Folding the lock into ComputeContactDatesAfterDelete's
-- CTE is NOT sufficient: FOR UPDATE re-reads only the locked contact row at
-- the post-wait snapshot, while the interaction aggregate in the same
-- statement still sees the statement's original snapshot, so a concurrently
-- committed interaction would be missed. Returns db.ErrNotFound (pgx.ErrNoRows)
-- when the contact was soft-deleted.
SELECT id FROM contact
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
FOR UPDATE;

-- name: TestLockContactForUpdateNoWait :one
-- TEST ONLY. Probe a contact row with FOR UPDATE NOWAIT: fails immediately
-- (lock_not_available) when another tx holds a conflicting lock on the row.
-- Used by the recompute lock-ordering regression test to prove (without a
-- sleep) that LockContactForDateRecompute is acquired as a blocking statement.
-- Production code must NOT call this.
SELECT id FROM contact
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
FOR UPDATE NOWAIT;

-- name: ComputeContactDatesAfterDelete :one
-- Returns the surgically-recomputed timestamp columns (each touched ONLY
-- when the deleted interaction at @deleted_at_ts was its source: column =
-- @deleted_at_ts → MAX(remaining live interactions of its subset), NULL when
-- none remain; otherwise the existing value is preserved) plus the fields the
-- Go caller needs to decide contact_by (old_last_contacted, old_contact_by,
-- cadence, created_at). Direction subsets mirror CadenceApplyFlagsByDirection
-- in reverse. NO cadence/contact_by arithmetic here — that is computed in Go
-- via cadence.CalculateContactBy to match the forward writer exactly.
-- MUST be preceded in the same tx by LockContactForDateRecompute so the
-- interaction aggregate is computed at a snapshot that already reflects any
-- concurrent writer that committed while waiting for the contact lock (the
-- FOR UPDATE retained below re-takes the held lock; the load-bearing
-- serialization is the prior LockContactForDateRecompute statement).
WITH agg AS (
  SELECT
    MAX(occurred_at) FILTER (WHERE direction IN ('inbound','mutual'))  AS new_non_outbound,
    MAX(occurred_at) FILTER (WHERE direction IN ('outbound','mutual')) AS new_outreach
  FROM interaction
  WHERE contact_id = sqlc.arg(id) AND deleted_at IS NULL
),
locked AS (
  SELECT id, last_contacted, last_interaction_at, last_response_at,
         last_outreach_at, contact_by, cadence, created_at
  FROM contact
  WHERE id = sqlc.arg(id) AND deleted_at IS NULL
  FOR UPDATE
)
SELECT
  agg.new_non_outbound AS new_non_outbound,
  agg.new_outreach     AS new_outreach,
  c.last_contacted     AS old_last_contacted,
  c.last_interaction_at AS old_last_interaction_at,
  c.last_response_at   AS old_last_response_at,
  c.last_outreach_at   AS old_last_outreach_at,
  c.contact_by     AS old_contact_by,
  c.cadence        AS cadence,
  c.created_at     AS created_at
FROM locked c, agg;

-- name: WriteContactDatesAfterDelete :exec
-- Writes the recomputed date columns. contact_by is passed pre-computed by
-- the Go caller (cadence.CalculateContactBy, environment-aware) so the value
-- matches the forward writer exactly; this query does no cadence arithmetic.
UPDATE contact SET
  last_contacted      = sqlc.narg(new_last_contacted)::timestamptz,
  last_interaction_at = sqlc.narg(new_last_interaction_at)::timestamptz,
  last_response_at    = sqlc.narg(new_last_response_at)::timestamptz,
  last_outreach_at    = sqlc.narg(new_last_outreach_at)::timestamptz,
  contact_by          = sqlc.narg(new_contact_by)::date,
  updated_at = NOW()
WHERE id = sqlc.arg(id) AND deleted_at IS NULL;

-- name: SetDerivedWriter :exec
-- Declares which owner the CALLING TRANSACTION is authorized to write derived
-- contact columns for. set_config(..., true) is the function form of
-- SET LOCAL: the value lives exactly as long as the transaction, so it can
-- never leak to the next checkout of this pooled connection. Read back by the
-- reject_unauthorized_derived_contact_write trigger (migration 079) via
-- current_setting('crm.derived_writer', true).
--
-- Callers MUST be inside a transaction. Issued at pool scope this sets the
-- value for a single implicit transaction and is then discarded, which is
-- silently useless rather than an error — hence repository.SetDerivedWriterTx
-- takes a pgx.Tx rather than a Querier.
SELECT set_config('crm.derived_writer', sqlc.arg(owner)::text, true);

-- name: ListContactsWithKnowledgeColumns :many
-- Backfill source for --migrate-contact-knowledge-columns: every non-deleted
-- contact whose location/birthday/how_met cache column still holds a value, so
-- the migration can mirror each into the assertion store with the contact's
-- created_at as the knowledge time. A soft-deleted contact is excluded (the
-- write API rejects a deleted subject node, matching the tag migration's
-- permanent skip). Deterministic order keeps a re-run's logs comparable.
SELECT id, location, birthday, how_met, created_at
FROM contact
WHERE deleted_at IS NULL
  AND (
       (location IS NOT NULL AND location != '')
    OR birthday IS NOT NULL
    OR (how_met IS NOT NULL AND how_met != '')
  )
ORDER BY created_at ASC, id ASC;
