-- Contact Task Queries (for Todoist cadence sync)

-- name: GetContactTask :one
SELECT * FROM contact_task
WHERE id = $1;

-- name: GetContactTaskByContact :one
-- Get the task for a specific contact+provider+kind combination
SELECT * FROM contact_task
WHERE contact_id = $1 AND provider = $2 AND kind = $3;

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
-- List tasks for a contact with optional state and kind filters
SELECT * FROM contact_task
WHERE contact_id = $1
  AND (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state')::text)
  AND (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind')::text)
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
    external_task_id,
    state,
    metadata
) VALUES (
    @contact_id,
    @provider,
    @kind,
    @external_task_id,
    COALESCE(@state, 'managed'),
    COALESCE(@metadata::jsonb, '{}'::jsonb)
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
    external_task_id,
    state,
    metadata,
    idempotency_key
) VALUES (
    @contact_id,
    @provider,
    @kind,
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
    external_task_id,
    state,
    metadata
) VALUES (
    @contact_id,
    @provider,
    @kind,
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

-- name: DeleteContactTaskByContact :exec
-- Delete task link for a contact+provider+kind (e.g., when cadence is disabled)
DELETE FROM contact_task
WHERE contact_id = $1 AND provider = $2 AND kind = $3;

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
  AND kind = 'follow_up'
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
  AND kind = 'follow_up'
  AND state IN ('managed', 'pending_remote_create')
RETURNING *;

-- name: FindPendingFollowUpTx :one
-- Transactional sibling of FindPendingFollowUp. Matches both 'managed'
-- and 'pending_remote_create' live states so the future cutover's
-- two-step creation is visible to this guard. Used by the
-- FollowUpManager consumer when running inside a worker transaction.
SELECT * FROM contact_task
WHERE contact_id = $1
  AND kind = 'follow_up'
  AND state IN ('managed', 'pending_remote_create')
LIMIT 1;

-- name: GetContactTaskByIdempotencyKey :one
-- Partial-index lookup for the local idempotency key used by the
-- follow-up consumer's crash-safe two-step creation. Matches the
-- partial unique index on (contact_id, kind, idempotency_key)
-- WHERE idempotency_key IS NOT NULL.
SELECT * FROM contact_task
WHERE contact_id = sqlc.arg('contact_id')
  AND kind = sqlc.arg('kind')
  AND idempotency_key = sqlc.arg('idempotency_key');

-- name: CountRiverJobsByContactTask :one
-- Test-only count of river_job rows with a given kind whose args JSON
-- contains contact_task_id = $2. Used by follow-up cutover integration
-- tests to assert a create/close/refresh job was enqueued without
-- inlining raw SQL into Go test code (core.md rule 2).
SELECT COUNT(*) FROM river_job
WHERE kind = sqlc.arg('kind')::text
  AND (args->>'contact_task_id') = sqlc.arg('contact_task_id')::text;
