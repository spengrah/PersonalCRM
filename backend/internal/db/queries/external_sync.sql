-- External Sync State Queries

-- name: GetSyncState :one
SELECT * FROM external_sync_state
WHERE id = $1;

-- name: GetSyncStateBySource :one
SELECT * FROM external_sync_state
WHERE source = $1 AND COALESCE(account_id, '') = COALESCE($2, '');

-- name: ListSyncStates :many
SELECT * FROM external_sync_state
ORDER BY source, account_id;

-- name: ListEnabledSyncStates :many
SELECT * FROM external_sync_state
WHERE enabled = TRUE AND status != 'disabled'
ORDER BY source, account_id;

-- name: ListDueSyncStates :many
-- The 'syncing' status value stays in the CHECK constraint for
-- down-migration safety but is no longer written by any code path.
-- River job state (available/running/completed/retryable) is the
-- source of truth for "in-flight" — this query only needs to filter
-- out 'disabled' rows whose next_sync_at has come due.
SELECT * FROM external_sync_state
WHERE enabled = TRUE
  AND status != 'disabled'
  AND (next_sync_at IS NULL OR next_sync_at <= $1)
ORDER BY next_sync_at ASC NULLS FIRST;

-- name: AbandonRunningLogsForState :exec
-- Called at the start of a retry attempt: marks any pre-existing 'running'
-- log row for this sync_state as 'abandoned' so that the new retry attempt
-- can insert a fresh log row without leaving orphan 'running' rows behind.
-- Requires migration 037 (widens the status CHECK).
UPDATE external_sync_log
SET completed_at = NOW(),
    status = 'abandoned',
    error_message = 'abandoned by retry; worker did not finish'
WHERE sync_state_id = $1 AND status = 'running';

-- name: CountInFlightSyncJobs :one
-- Counts river_job rows that represent an in-flight SyncProviderAccountJob
-- for the given (source, account_id). Used by
-- EnqueueAccountSyncIfNotInFlight as a pre-insert dedup check inside the
-- advisory-lock transaction. The args->>'source' and args->>'account_id'
-- JSONB paths match the struct tags on scheduler.SyncProviderAccountArgs;
-- TestSyncProviderAccountArgs_JSONContract guards the key names.
SELECT COUNT(*) FROM river_job
WHERE kind = 'sync_provider_account'
  AND state IN ('available', 'pending', 'running', 'retryable', 'scheduled')
  AND (args->>'source') = sqlc.arg('source')::text
  AND COALESCE(args->>'account_id', '') = COALESCE(sqlc.narg('account_id')::text, '');

-- name: CreateSyncState :one
INSERT INTO external_sync_state (
    source,
    account_id,
    enabled,
    status,
    strategy,
    next_sync_at,
    metadata
) VALUES (
    @source,
    @account_id,
    @enabled,
    COALESCE(@status, 'idle'),
    COALESCE(@strategy, 'contact_driven'),
    @next_sync_at,
    COALESCE(@metadata::jsonb, '{}'::jsonb)
) RETURNING *;

-- name: UpdateSyncStateStatus :one
UPDATE external_sync_state
SET status = $2,
    error_message = CASE WHEN $2 = 'error' THEN $3 ELSE NULL END,
    error_count = CASE WHEN $2 = 'error' THEN error_count + 1 ELSE 0 END,
    last_sync_at = NOW(),
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateSyncStateSuccess :one
UPDATE external_sync_state
SET status = 'idle',
    last_sync_at = NOW(),
    last_successful_sync_at = NOW(),
    next_sync_at = $2,
    sync_cursor = $3,
    error_message = NULL,
    error_count = 0,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateSyncStateNextSync :exec
UPDATE external_sync_state
SET next_sync_at = $2,
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateSyncStateEnabled :one
UPDATE external_sync_state
SET enabled = $2,
    status = CASE WHEN $2 = FALSE THEN 'disabled' ELSE 'idle' END,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateSyncStateCursor :exec
UPDATE external_sync_state
SET sync_cursor = $2,
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateSyncStateMetadata :one
UPDATE external_sync_state
SET metadata = $2,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteSyncState :exec
DELETE FROM external_sync_state
WHERE id = $1;

-- name: DeleteSyncStatesByAccountID :exec
DELETE FROM external_sync_state
WHERE account_id IS NOT NULL AND account_id = $1;

-- Mac-daemon push-cursor queries (see backend/internal/repository/mac_host.go).
-- Cursors for push providers live in external_sync_state keyed by
-- (source, account_id) where account_id = mac_host.id. cursor_epoch is
-- read from mac_host (see GetMacHostCursorEpoch) — the row lock there
-- serializes concurrent commits for the same host.

-- name: GetMacHostSyncState :one
-- Returns the push-cursor row for (source, host_id). The handler treats
-- db.ErrNotFound as "no cursor committed yet" and surfaces an empty
-- cursor + the host's current cursor_epoch.
SELECT * FROM external_sync_state
WHERE source = @source
  AND COALESCE(account_id, '') = @account_id::text
  AND strategy = 'push';

-- name: InsertMacHostSyncCursor :one
-- First-commit insert path. ON CONFLICT DO NOTHING handles the
-- concurrent-first-write race: the loser gets zero rows, re-reads the
-- now-committed state, and surfaces ErrCursorBaseMismatch with the
-- winner's cursor.
INSERT INTO external_sync_state
    (source, account_id, enabled, status, strategy, sync_cursor, next_sync_at)
VALUES (@source, @account_id, TRUE, 'idle', 'push', @new_cursor, NULL)
ON CONFLICT (source, COALESCE(account_id, '')) DO NOTHING
RETURNING id, sync_cursor;

-- name: UpdateMacHostSyncCursor :one
-- CAS-style update: only updates when sync_cursor matches base_cursor.
-- Zero rows returned means another writer slipped in between the
-- planning read and this update; the caller re-reads and surfaces 409.
UPDATE external_sync_state
SET sync_cursor = @new_cursor,
    updated_at  = NOW()
WHERE id = @id
  AND COALESCE(sync_cursor, '') = @base_cursor::text
RETURNING id, sync_cursor;

-- name: DeleteMacHostSyncStates :execrows
-- Cascade-on-revoke: when a host is uninstalled, drop its push-cursor
-- rows. Filters by strategy='push' so OAuth-driven rows that happen to
-- share an account_id string can never be deleted by this path (defence
-- in depth; account_id collisions between Mac UUIDs and OAuth account
-- emails are not possible by format but the explicit strategy filter
-- locks the contract).
DELETE FROM external_sync_state
WHERE strategy = 'push' AND COALESCE(account_id, '') = @account_id::text;

-- External Sync Log Queries

-- name: CreateSyncLog :one
INSERT INTO external_sync_log (
    sync_state_id,
    source,
    account_id,
    status,
    metadata
) VALUES (
    @sync_state_id,
    @source,
    @account_id,
    'running',
    COALESCE(@metadata::jsonb, '{}'::jsonb)
) RETURNING *;

-- name: CompleteSyncLog :one
UPDATE external_sync_log
SET completed_at = NOW(),
    status = $2,
    items_processed = $3,
    items_matched = $4,
    items_created = $5,
    error_message = $6
WHERE id = $1
RETURNING *;

-- name: GetSyncLog :one
SELECT * FROM external_sync_log
WHERE id = $1;

-- name: ListSyncLogsByState :many
SELECT * FROM external_sync_log
WHERE sync_state_id = $1
ORDER BY started_at DESC
LIMIT $2 OFFSET $3;

-- name: ListRecentSyncLogs :many
SELECT * FROM external_sync_log
ORDER BY started_at DESC
LIMIT $1;

-- name: CountSyncLogsByState :one
SELECT COUNT(*) FROM external_sync_log
WHERE sync_state_id = $1;

-- name: DeleteOldSyncLogs :exec
DELETE FROM external_sync_log
WHERE created_at < $1;
