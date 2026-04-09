-- name: UpsertTelegramMessage :one
INSERT INTO telegram_message (
    telegram_message_id, telegram_chat_id, chat_type, chat_title,
    message_text, message_type, sent_at, edited_at, is_outgoing,
    reply_to_msg_id, peer_user_id, peer_username, peer_first_name,
    peer_last_name, peer_phone
) VALUES (
    @telegram_message_id, @telegram_chat_id, @chat_type, @chat_title,
    @message_text, @message_type, @sent_at, @edited_at, @is_outgoing,
    @reply_to_msg_id, @peer_user_id, @peer_username, @peer_first_name,
    @peer_last_name, @peer_phone
)
ON CONFLICT (telegram_chat_id, telegram_message_id) DO UPDATE SET
    message_text = EXCLUDED.message_text,
    edited_at = COALESCE(EXCLUDED.edited_at, telegram_message.edited_at),
    peer_username = COALESCE(EXCLUDED.peer_username, telegram_message.peer_username),
    peer_first_name = COALESCE(EXCLUDED.peer_first_name, telegram_message.peer_first_name),
    peer_last_name = COALESCE(EXCLUDED.peer_last_name, telegram_message.peer_last_name),
    peer_phone = COALESCE(EXCLUDED.peer_phone, telegram_message.peer_phone)
RETURNING *;

-- name: SoftDeleteTelegramMessages :exec
UPDATE telegram_message
SET deleted_at = NOW()
WHERE telegram_message_id = ANY(@message_ids::int[])
  AND deleted_at IS NULL;

-- name: SoftDeleteTelegramChannelMessages :exec
UPDATE telegram_message
SET deleted_at = NOW()
WHERE telegram_chat_id = @telegram_chat_id
  AND telegram_message_id = ANY(@message_ids::int[])
  AND deleted_at IS NULL;

-- name: GetTelegramMessage :one
SELECT * FROM telegram_message
WHERE telegram_chat_id = @telegram_chat_id
  AND telegram_message_id = @telegram_message_id
  AND deleted_at IS NULL;

-- name: ListTelegramMessagesByChatUnprocessed :many
SELECT * FROM telegram_message
WHERE telegram_chat_id = @telegram_chat_id
  AND processed_at IS NULL
  AND deleted_at IS NULL
ORDER BY sent_at;

-- name: CountTelegramMessagesByChat :many
SELECT telegram_chat_id, COUNT(*) as message_count
FROM telegram_message
WHERE deleted_at IS NULL
GROUP BY telegram_chat_id;
