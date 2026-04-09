-- name: GetTelegramChatConfig :one
SELECT * FROM telegram_chat_config WHERE telegram_chat_id = @telegram_chat_id;

-- name: UpsertTelegramChatConfig :one
INSERT INTO telegram_chat_config (
    telegram_chat_id, chat_title, chat_type, member_count, status
) VALUES (
    @telegram_chat_id, @chat_title, @chat_type, @member_count, @status
)
ON CONFLICT (telegram_chat_id) DO UPDATE SET
    chat_title = COALESCE(EXCLUDED.chat_title, telegram_chat_config.chat_title),
    member_count = COALESCE(EXCLUDED.member_count, telegram_chat_config.member_count),
    status = telegram_chat_config.status,
    updated_at = NOW()
RETURNING *;

-- name: UpdateTelegramChatConfigStatus :one
UPDATE telegram_chat_config
SET status = @status, updated_at = NOW()
WHERE telegram_chat_id = @telegram_chat_id
RETURNING *;

-- name: ListTelegramChatConfigs :many
SELECT * FROM telegram_chat_config ORDER BY chat_title;

-- name: UpdateTelegramChatConfigBackfillCursor :exec
UPDATE telegram_chat_config
SET backfill_cursor = @backfill_cursor, updated_at = NOW()
WHERE telegram_chat_id = @telegram_chat_id;

-- name: UpdateTelegramChatConfigBackfillComplete :exec
UPDATE telegram_chat_config
SET backfill_complete = TRUE, backfill_cursor = NULL, updated_at = NOW()
WHERE telegram_chat_id = @telegram_chat_id;

-- name: ResetTelegramChatConfigBackfill :exec
UPDATE telegram_chat_config
SET backfill_complete = FALSE, backfill_cursor = NULL, updated_at = NOW()
WHERE telegram_chat_id = @telegram_chat_id;

-- name: ListTelegramChatConfigsForBackfill :many
SELECT * FROM telegram_chat_config
WHERE backfill_complete = FALSE
ORDER BY created_at;

-- name: UpdateTelegramChatConfigMemberCount :exec
UPDATE telegram_chat_config
SET member_count = @member_count, updated_at = NOW()
WHERE telegram_chat_id = @telegram_chat_id;

-- name: DeleteTelegramChatConfig :exec
DELETE FROM telegram_chat_config WHERE telegram_chat_id = @telegram_chat_id;
