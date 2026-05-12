-- mac_host queries.

-- name: CreateMacHost :one
INSERT INTO mac_host (
    hostname,
    daemon_version,
    protocol_version,
    api_key_hash
) VALUES (
    @hostname,
    @daemon_version,
    @protocol_version,
    @api_key_hash
)
RETURNING *;

-- name: GetMacHost :one
SELECT * FROM mac_host WHERE id = $1;

-- name: GetActiveMacHostByID :one
-- Used by MacHostAuthMiddleware. Filters revoked hosts so a revoked
-- daemon's bearer key cannot authenticate.
SELECT * FROM mac_host
WHERE id = $1 AND api_key_revoked_at IS NULL;

-- name: ListMacHosts :many
SELECT * FROM mac_host
ORDER BY created_at DESC;

-- name: ListActiveMacHosts :many
SELECT * FROM mac_host
WHERE api_key_revoked_at IS NULL
ORDER BY created_at DESC;

-- name: RevokeMacHost :one
UPDATE mac_host
SET api_key_revoked_at = NOW()
WHERE id = $1 AND api_key_revoked_at IS NULL
RETURNING *;

-- name: UpdateMacHostHeartbeat :one
UPDATE mac_host
SET last_heartbeat_at  = NOW(),
    daemon_version     = @daemon_version,
    protocol_version   = @protocol_version,
    permissions        = @permissions,
    source_health      = @source_health
WHERE id = @id AND api_key_revoked_at IS NULL
RETURNING *;

-- name: GetMacHostCursorEpoch :one
-- Reads the host's cursor_epoch with a row lock. Used by the cursor
-- commit path to serialize concurrent commits per host and to bind
-- the epoch read against the cursor read in the same tx.
SELECT id, cursor_epoch, api_key_revoked_at
FROM mac_host
WHERE id = $1
FOR UPDATE;

-- name: BumpMacHostCursorEpoch :one
-- Admin operation (not exposed in PR1). Bumps cursor_epoch so the daemon
-- discards its local cursor cache on next heartbeat.
UPDATE mac_host
SET cursor_epoch = cursor_epoch + 1
WHERE id = $1 AND api_key_revoked_at IS NULL
RETURNING cursor_epoch;

-- mac_host_pairing_token queries.

-- name: CreatePairingToken :one
INSERT INTO mac_host_pairing_token (token_hash, expires_at)
VALUES (@token_hash, @expires_at)
RETURNING *;

-- name: GetPairingTokenByHashForUpdate :one
-- Locks the row so the consume path serializes against concurrent
-- consume attempts for the same token.
SELECT * FROM mac_host_pairing_token
WHERE token_hash = $1
FOR UPDATE;

-- name: MarkPairingTokenConsumed :one
UPDATE mac_host_pairing_token
SET consumed_at = NOW(),
    consumed_host_id = @consumed_host_id
WHERE id = @id
RETURNING *;

-- name: DeleteExpiredPairingTokens :execrows
DELETE FROM mac_host_pairing_token
WHERE consumed_at IS NULL AND expires_at < NOW();
