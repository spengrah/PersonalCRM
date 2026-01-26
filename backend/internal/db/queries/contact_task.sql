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

-- name: UpsertContactTask :one
-- Upsert a contact task (update external_task_id if exists)
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
) ON CONFLICT (contact_id, provider, kind) DO UPDATE SET
    external_task_id = EXCLUDED.external_task_id,
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
-- Update the external task ID (when creating a new Todoist task)
UPDATE contact_task
SET external_task_id = $2,
    state = 'managed',
    updated_at = NOW()
WHERE id = $1
RETURNING *;

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
