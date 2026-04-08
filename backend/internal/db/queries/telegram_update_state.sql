-- name: GetTelegramUpdateState :one
SELECT * FROM telegram_update_state WHERE user_id = @user_id;

-- name: UpsertTelegramUpdateState :one
INSERT INTO telegram_update_state (user_id, pts, qts, seq, date)
VALUES (@user_id, @pts, @qts, @seq, @date)
ON CONFLICT (user_id) DO UPDATE SET
    pts = EXCLUDED.pts,
    qts = EXCLUDED.qts,
    seq = EXCLUDED.seq,
    date = EXCLUDED.date,
    updated_at = NOW()
RETURNING *;

-- name: SetTelegramPts :exec
UPDATE telegram_update_state SET pts = @pts, updated_at = NOW() WHERE user_id = @user_id;

-- name: SetTelegramQts :exec
UPDATE telegram_update_state SET qts = @qts, updated_at = NOW() WHERE user_id = @user_id;

-- name: SetTelegramSeq :exec
UPDATE telegram_update_state SET seq = @seq, updated_at = NOW() WHERE user_id = @user_id;

-- name: SetTelegramDate :exec
UPDATE telegram_update_state SET date = @date, updated_at = NOW() WHERE user_id = @user_id;

-- name: DeleteTelegramUpdateState :exec
DELETE FROM telegram_update_state WHERE user_id = @user_id;
