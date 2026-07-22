-- Contact Task Queries (for Todoist cadence sync)

-- name: GetContactTask :one
SELECT * FROM contact_task
WHERE id = $1;

-- name: GetContactTaskByContactCadenceDue :one
-- Look up the cadence-due task for a contact+provider. Backed by the
-- partial unique index unique_contact_provider_cadence
-- (lifecycle='cadence_due', no state filter), so at most one row exists.
SELECT * FROM contact_task
WHERE contact_id = sqlc.arg('contact_id')
  AND provider = sqlc.arg('provider')
  AND lifecycle = 'cadence_due';

-- name: GetContactTaskByContactFollowUpLive :one
-- Look up the live follow-up task for a contact+provider. Backed by the
-- partial unique index idx_contact_task_followup_unique_live
-- (lifecycle='followup_loop' AND state IN live states).
SELECT * FROM contact_task
WHERE contact_id = sqlc.arg('contact_id')
  AND provider = sqlc.arg('provider')
  AND lifecycle = 'followup_loop'
  AND state IN ('managed', 'pending_remote_create');

-- name: GetLegacyActionTaskByContact :one
-- Legacy lookup for action-kind rows (no new creator path; preserved for
-- pre-migration rows). Multiple action rows are possible per
-- contact/provider; this returns the most recent.
SELECT * FROM contact_task
WHERE contact_id = sqlc.arg('contact_id')
  AND provider = sqlc.arg('provider')
  AND kind = 'action'
ORDER BY created_at DESC
LIMIT 1;

-- name: GetContactTaskByExternalID :one
-- Look up a task by its external provider ID
SELECT * FROM contact_task
WHERE provider = $1 AND external_task_id = $2;

-- name: ListContactTasksByProvider :many
-- List all tasks for a provider (optionally filtered by state)
SELECT * FROM contact_task
WHERE provider = $1
  AND (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state')::text)
ORDER BY created_at DESC;

-- name: ListContactTasksByContact :many
-- List all tasks for a contact
SELECT * FROM contact_task
WHERE contact_id = $1
ORDER BY provider, kind;

-- name: ListContactTasksByContactFiltered :many
-- List tasks for a contact with optional state, kind, and lifecycle filters
SELECT * FROM contact_task
WHERE contact_id = sqlc.arg('contact_id')
  AND (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state')::text)
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text)
  AND (sqlc.narg('lifecycle')::text IS NULL OR lifecycle = sqlc.narg('lifecycle')::text)
ORDER BY created_at DESC;

-- name: ListManagedContactTasks :many
-- List all managed tasks for a provider (for reconciliation)
SELECT ct.*, c.full_name, c.cadence, c.contact_by, c.last_contacted
FROM contact_task ct
JOIN contact c ON c.id = ct.contact_id AND c.deleted_at IS NULL
WHERE ct.provider = $1 AND ct.state = 'managed'
ORDER BY ct.created_at;

-- name: CreateContactTask :one
INSERT INTO contact_task (
    contact_id,
    provider,
    kind,
    lifecycle,
    external_task_id,
    state,
    metadata
) VALUES (
    @contact_id,
    @provider,
    @kind,
    @lifecycle,
    @external_task_id,
    COALESCE(@state, 'managed'),
    COALESCE(@metadata::jsonb, '{}'::jsonb)
) RETURNING *;

-- name: CreateContactTaskAtTime :one
-- Seed-only variant of CreateContactTask that sets created_at explicitly, so the
-- synthetic seeder can vary a linked task's created_at (its "link age")
-- anchor-relatively without a raw SQL insert. Production creators always let
-- created_at default to NOW(); no request path calls this.
INSERT INTO contact_task (
    contact_id,
    provider,
    kind,
    lifecycle,
    external_task_id,
    state,
    metadata,
    created_at
) VALUES (
    @contact_id,
    @provider,
    @kind,
    @lifecycle,
    @external_task_id,
    COALESCE(@state, 'managed'),
    COALESCE(@metadata::jsonb, '{}'::jsonb),
    @created_at
) RETURNING *;

-- name: CreateContactTaskWithIdempotencyKey :one
-- Variant of CreateContactTask that accepts an explicit idempotency_key,
-- used by the cutover FollowUpManager for crash-safe two-step create.
-- A NULL key is permitted so the generic path can reuse the query shape;
-- non-NULL keys collide with the partial unique index
-- idx_contact_task_followup_idempotency on repeats.
INSERT INTO contact_task (
    contact_id,
    provider,
    kind,
    lifecycle,
    external_task_id,
    state,
    metadata,
    idempotency_key
) VALUES (
    @contact_id,
    @provider,
    @kind,
    @lifecycle,
    @external_task_id,
    COALESCE(@state, 'managed'),
    COALESCE(@metadata::jsonb, '{}'::jsonb),
    sqlc.narg('idempotency_key')
) RETURNING *;

-- name: UpsertContactTask :one
-- Upsert a contact task by external_task_id (Todoist task IDs are globally unique)
INSERT INTO contact_task (
    contact_id,
    provider,
    kind,
    lifecycle,
    external_task_id,
    state,
    metadata
) VALUES (
    @contact_id,
    @provider,
    @kind,
    @lifecycle,
    @external_task_id,
    COALESCE(@state, 'managed'),
    COALESCE(@metadata::jsonb, '{}'::jsonb)
) ON CONFLICT (external_task_id) WHERE external_task_id <> '' DO UPDATE SET
    state = EXCLUDED.state,
    metadata = EXCLUDED.metadata,
    updated_at = NOW()
RETURNING *;

-- name: UpdateContactTaskState :one
UPDATE contact_task
SET state = $2,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateContactTaskExternalID :one
-- Update the external task ID (when creating a new Todoist task).
-- Finalizes the two-step follow-up create: transitions a
-- pending_remote_create row to state='managed' once the remote
-- task ID is known. Use SetContactTaskExternalIDOnly when the row
-- should stay in its current state (close-while-pending race).
UPDATE contact_task
SET external_task_id = $2,
    state = 'managed',
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: SetContactTaskExternalIDOnly :exec
-- Persist external_task_id on a row without touching state. Used by the
-- close-while-pending race path: the create worker enters on a row that
-- has already transitioned to 'completed' (inbound arrived mid-flight),
-- issues item_add to keep Todoist in sync, and records the resulting ID
-- without flipping the row back to 'managed'.
UPDATE contact_task
SET external_task_id = $2,
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateContactTaskMetadata :one
UPDATE contact_task
SET metadata = $2,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteContactTask :exec
DELETE FROM contact_task
WHERE id = $1;

-- name: DeleteContactTaskByContactCadenceDue :exec
-- Delete the cadence-due task link for a contact+provider (e.g., when
-- cadence is disabled). Uses the lifecycle predicate to match the
-- post-046 schema.
DELETE FROM contact_task
WHERE contact_id = sqlc.arg('contact_id')
  AND provider = sqlc.arg('provider')
  AND lifecycle = 'cadence_due';

-- name: DeleteContactTasksByProvider :exec
-- Delete all tasks for a provider (e.g., when disconnecting Todoist)
DELETE FROM contact_task
WHERE provider = $1;

-- name: CountContactTasksByProvider :one
SELECT COUNT(*) FROM contact_task
WHERE provider = $1 AND state = $2;

-- name: GetContactTaskByPendingTempID :one
-- Find a task by its pending temp ID in metadata (for mapping temp IDs to real Todoist IDs)
SELECT * FROM contact_task
WHERE provider = @provider AND metadata->>'pending_temp_id' = @temp_id::text;

-- name: FindPendingFollowUp :one
-- Find a pending follow-up task for a contact. Matches both 'managed'
-- and 'pending_remote_create' live states so the two-step create flow
-- is visible to non-tx callers (e.g. Todoist provider's closeOnOutreach).
SELECT * FROM contact_task
WHERE contact_id = $1
  AND lifecycle = 'followup_loop'
  AND state IN ('managed', 'pending_remote_create')
LIMIT 1;

-- name: CompleteFollowUpForContact :many
-- Mark all pending follow-up tasks as completed for a contact (when a
-- response arrives). Matches the same live-state set as FindPendingFollowUp
-- so an inbound arriving while the create worker is mid-flight still
-- transitions the row to completed.
UPDATE contact_task
SET state = 'completed',
    updated_at = NOW()
WHERE contact_id = $1
  AND lifecycle = 'followup_loop'
  AND state IN ('managed', 'pending_remote_create')
RETURNING *;

-- name: CompleteLiveContactTasksForContact :many
-- Merge-time close of the source contact's live AUTOMATED tasks (cadence_due
-- + followup_loop). Manual-lifecycle rows are deliberately excluded — they
-- are user content and are REPOINTED to the merge target instead (see
-- RepointManualContactTasksToContact). Automated rows cannot be repointed:
-- unique_contact_provider_cadence has no state filter, so a repoint collides
-- whenever the target has ANY cadence_due row, and the target's own automated
-- rows already cover the survivor. Returns the closed rows' identifying
-- fields plus the pending_temp_id metadata key so the service can decide
-- remote-close enqueue eligibility (real external id only — never a pending
-- temp id, mirroring todoist.isPendingTempID).
UPDATE contact_task
SET state = 'completed',
    updated_at = NOW()
WHERE contact_id = @contact_id
  AND lifecycle IN ('cadence_due', 'followup_loop')
  AND state IN ('managed', 'pending_remote_create')
RETURNING id, external_task_id, provider,
    COALESCE(metadata->>sqlc.arg(pending_temp_id_key)::text, '')::text AS pending_temp_id;

-- name: RepointManualContactTasksToContact :exec
-- Merge-time repoint of the source contact's MANUAL tasks (user to-dos) to
-- the target, all states — closing one would silently check off the user's
-- live task, and leaving it would let the Todoist sync's contact-deleted
-- branch hard-DELETE the remote task. Collision-free: neither partial unique
-- index covers lifecycle='manual', and unique_external_task_id keys on
-- external_task_id, which this update does not touch. Repointing terminal
-- states too keeps the survivor's task history intact (mirrors
-- TransferInteractions).
UPDATE contact_task
SET contact_id = sqlc.arg(target_contact_id),
    updated_at = NOW()
WHERE contact_id = sqlc.arg(source_contact_id)
  AND lifecycle = 'manual';

-- name: FindPendingFollowUpTx :one
-- Transactional sibling of FindPendingFollowUp. Matches both 'managed'
-- and 'pending_remote_create' live states so the future cutover's
-- two-step creation is visible to this guard. Used by the
-- FollowUpManager consumer when running inside a worker transaction.
SELECT * FROM contact_task
WHERE contact_id = $1
  AND lifecycle = 'followup_loop'
  AND state IN ('managed', 'pending_remote_create')
LIMIT 1;

-- name: GetContactTaskByIdempotencyKey :one
-- Partial-index lookup for the local idempotency key used by the
-- follow-up consumer's crash-safe two-step creation. Matches the
-- partial unique index on (contact_id, idempotency_key)
-- WHERE lifecycle = 'followup_loop' AND idempotency_key IS NOT NULL.
SELECT * FROM contact_task
WHERE contact_id = sqlc.arg('contact_id')
  AND lifecycle = 'followup_loop'
  AND idempotency_key = sqlc.arg('idempotency_key');

-- name: CountRiverJobsByContactTask :one
-- Test-only count of river_job rows with a given kind whose args JSON
-- contains contact_task_id = $2. Used by follow-up cutover integration
-- tests to assert a create/close/refresh job was enqueued without
-- inlining raw SQL into Go test code (core.md rule 2).
SELECT COUNT(*) FROM river_job
WHERE kind = sqlc.arg('kind')::text
  AND (args->>'contact_task_id') = sqlc.arg('contact_task_id')::text;

-- name: CountTodoistOpJobsByOp :one
-- Test-only count of todoist_task_op river_job rows for a given
-- contact_task_id and op verb. Used by the follow-up cutover integration
-- tests to assert a create/close/update_deadline op was (or was not)
-- enqueued, without inlining raw SQL into Go test code (core.md rule 2).
SELECT COUNT(*) FROM river_job
WHERE kind = 'todoist_task_op'
  AND (args->>'contact_task_id') = sqlc.arg('contact_task_id')::text
  AND (args->>'op') = sqlc.arg('op')::text;
