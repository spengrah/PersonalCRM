-- name: GetTelegramChannelState :one
SELECT * FROM telegram_channel_state WHERE channel_id = @channel_id;

-- name: UpsertTelegramChannelState :one
INSERT INTO telegram_channel_state (channel_id, pts, access_hash)
VALUES (@channel_id, @pts, @access_hash)
ON CONFLICT (channel_id) DO UPDATE SET
    pts = EXCLUDED.pts,
    access_hash = EXCLUDED.access_hash,
    updated_at = NOW()
RETURNING *;

-- name: UpsertTelegramChannelAccessHash :one
-- Used by ChannelAccessHasher — only updates access_hash, preserves existing pts.
INSERT INTO telegram_channel_state (channel_id, pts, access_hash)
VALUES (@channel_id, 0, @access_hash)
ON CONFLICT (channel_id) DO UPDATE SET
    access_hash = EXCLUDED.access_hash,
    updated_at = NOW()
RETURNING *;

-- name: SetTelegramChannelPts :exec
UPDATE telegram_channel_state SET pts = @pts, updated_at = NOW() WHERE channel_id = @channel_id;

-- name: ListTelegramChannelStates :many
SELECT * FROM telegram_channel_state;

-- name: DeleteTelegramChannelState :exec
DELETE FROM telegram_channel_state WHERE channel_id = @channel_id;
