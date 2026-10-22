-- name: ListMessageNameSpans :many
-- The stored spans of the model version for each (message key, text digest)
-- pair asked; a pair with none stored has no row.
SELECT s.message_key, s.text_digest, s.starts, s.ends
FROM message_name_span s
WHERE s.model_version = sqlc.arg(model_version)
  AND (s.message_key, s.text_digest) IN (
      SELECT unnest(sqlc.arg(message_keys)::text[]), unnest(sqlc.arg(text_digests)::bytea[]));

-- name: InsertMessageNameSpan :exec
-- Stores the spans of one text. The model's spans of a text are a function of
-- the text and the model version, so a row already stored is kept.
INSERT INTO message_name_span (message_key, text_digest, model_version, starts, ends)
VALUES (sqlc.arg(message_key), sqlc.arg(text_digest), sqlc.arg(model_version), sqlc.arg(starts)::integer[], sqlc.arg(ends)::integer[])
ON CONFLICT (message_key, text_digest, model_version) DO NOTHING;
