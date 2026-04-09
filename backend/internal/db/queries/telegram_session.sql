-- name: GetTelegramSession :one
SELECT * FROM telegram_session WHERE id = 1;

-- name: UpsertTelegramSession :one
INSERT INTO telegram_session (
    id, session_data_encrypted, encryption_nonce, phone_number,
    telegram_user_id, username, auth_state
) VALUES (
    1, @session_data_encrypted, @encryption_nonce, @phone_number,
    @telegram_user_id, @username, @auth_state
)
ON CONFLICT (id) DO UPDATE SET
    session_data_encrypted = EXCLUDED.session_data_encrypted,
    encryption_nonce = EXCLUDED.encryption_nonce,
    phone_number = COALESCE(EXCLUDED.phone_number, telegram_session.phone_number),
    telegram_user_id = COALESCE(EXCLUDED.telegram_user_id, telegram_session.telegram_user_id),
    username = COALESCE(EXCLUDED.username, telegram_session.username),
    auth_state = EXCLUDED.auth_state,
    updated_at = NOW()
RETURNING *;

-- name: UpdateTelegramSessionAuthState :one
UPDATE telegram_session
SET auth_state = @auth_state, updated_at = NOW()
WHERE id = 1
RETURNING *;

-- name: UpdateTelegramSessionUserInfo :one
UPDATE telegram_session
SET telegram_user_id = @telegram_user_id,
    username = @username,
    phone_number = @phone_number,
    updated_at = NOW()
WHERE id = 1
RETURNING *;

-- name: UpsertTelegramSessionData :one
-- Used by gotd/td session.Storage — only updates encrypted session data,
-- does NOT touch auth_state (which is managed by AuthSessionManager).
INSERT INTO telegram_session (id, session_data_encrypted, encryption_nonce, auth_state)
VALUES (1, @session_data_encrypted, @encryption_nonce, 'disconnected')
ON CONFLICT (id) DO UPDATE SET
    session_data_encrypted = EXCLUDED.session_data_encrypted,
    encryption_nonce = EXCLUDED.encryption_nonce,
    updated_at = NOW()
RETURNING *;

-- name: DeleteTelegramSession :exec
DELETE FROM telegram_session WHERE id = 1;
