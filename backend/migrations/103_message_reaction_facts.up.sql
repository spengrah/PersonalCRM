-- A message reaction's facts, stored on the reaction's own message.
--
-- messages_message gains the facts the Mac daemon sends with a reaction: its
-- kind, the emoji of an emoji reaction, the stored key of the message it
-- targets, and whether it takes a reaction back. A message is a reaction
-- exactly when it has a kind. The constraints make a partial or contradictory
-- fact set unstorable: the kind, target and removal flag are all present or
-- all absent, and an emoji is present exactly on the emoji kind. The
-- reaction_kind type is the one list of kinds: sqlc generates the Go values
-- that validate a reaction from it. Every existing row stores no facts.
--
-- thread_content (migration 096) gains the facts as four trailing columns, so
-- the thread-context read carries them beside each message. Every other column
-- is unchanged.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so multi-statement DDL must self-wrap to stay
-- all-or-nothing on a mid-file failure (Postgres DDL is transactional).

BEGIN;

CREATE TYPE reaction_kind AS ENUM (
    'love', 'like', 'dislike', 'laugh', 'emphasize', 'question', 'emoji', 'sticker');

ALTER TABLE messages_message
    ADD COLUMN reaction_kind reaction_kind,
    ADD COLUMN reaction_emoji TEXT,
    ADD COLUMN reaction_target_key TEXT,
    ADD COLUMN reaction_removal BOOLEAN,
    ADD CONSTRAINT messages_message_reaction_facts_check CHECK (
        (reaction_kind IS NULL) = (reaction_target_key IS NULL)
        AND (reaction_kind IS NULL) = (reaction_removal IS NULL)
        AND (reaction_kind IS NOT DISTINCT FROM 'emoji') = (reaction_emoji IS NOT NULL)
        AND reaction_target_key <> ''
        AND reaction_emoji <> '');

CREATE OR REPLACE VIEW thread_content AS
SELECT
    v.node_id AS venue_id,
    v.kind AS venue_kind,
    (cm.source || ':' || cm.external_id)::text AS message_key,
    cm.sent_at,
    cm.direction = 'outbound' AS from_user,
    -- An inbound row's peer is its sender. An outbound gchat row's peer is the
    -- user's own address, and an outbound group message names none; every other
    -- outbound peer is a recipient, which an email's address headers name beside
    -- its address.
    (CASE
        WHEN cm.direction = 'inbound'
        THEN cm.source || ':' || COALESCE(NULLIF(cm.peer_normalized, ''), cm.peer_handle, '')
        WHEN cm.source <> 'gchat'
        THEN COALESCE(cm.source || ':' || COALESCE(NULLIF(cm.peer_normalized, ''), NULLIF(cm.peer_handle, '')), '')
        ELSE ''
    END)::text AS peer_key,
    lc.id AS peer_contact_id,
    (CASE
        WHEN cm.direction = 'inbound' THEN array_remove(ARRAY[
            NULLIF(cm.source_metadata->>'from_name', ''),
            NULLIF(cm.source_metadata->>'push_name', ''),
            NULLIF(cm.source_metadata->>'from', ''),
            NULLIF(cm.peer_handle, '')
        ], NULL)
        WHEN cm.source = 'gchat' THEN ARRAY[]::text[]
        ELSE array_remove(ARRAY[NULLIF(cm.peer_handle, '')], NULL) || ARRAY(
            SELECT names.name
            FROM (VALUES ('to', 'to_names'), ('cc', 'cc_names'), ('bcc', 'bcc_names')) AS field(addresses, names)
            CROSS JOIN LATERAL jsonb_array_elements_text(CASE
                WHEN jsonb_typeof(cm.source_metadata->field.addresses) = 'array'
                THEN cm.source_metadata->field.addresses ELSE '[]'::jsonb END
            ) WITH ORDINALITY AS addresses(address, n)
            JOIN LATERAL jsonb_array_elements_text(CASE
                WHEN jsonb_typeof(cm.source_metadata->field.names) = 'array'
                THEN cm.source_metadata->field.names ELSE '[]'::jsonb END
            ) WITH ORDINALITY AS names(name, n) ON names.n = addresses.n
            WHERE lower(addresses.address) = lower(cm.peer_handle)
              AND names.name <> '')
    END)::text[] AS peer_aliases,
    (CASE WHEN cm.direction = 'inbound' THEN ARRAY[]::text[] ELSE array_remove(ARRAY[
        NULLIF(cm.source_metadata->>'from_name', ''),
        NULLIF(cm.source_metadata->>'push_name', ''),
        NULLIF(cm.source_metadata->>'from', ''),
        CASE WHEN cm.source = 'gchat' THEN NULLIF(cm.peer_handle, '') END
    ], NULL) END)::text[] AS user_aliases,
    COALESCE(cm.body, '') AS body,
    jsonb_strip_nulls(jsonb_build_object(
        'from', cm.source_metadata->'from',
        'from_name', cm.source_metadata->'from_name',
        'to', cm.source_metadata->'to',
        'to_names', cm.source_metadata->'to_names',
        'cc', cm.source_metadata->'cc',
        'cc_names', cm.source_metadata->'cc_names',
        'bcc', cm.source_metadata->'bcc',
        'bcc_names', cm.source_metadata->'bcc_names',
        'subject', to_jsonb(cm.subject)
    )) AS headers,
    cm.interaction_id AS observation_id,
    -- A reaction's facts, from the reaction's own stored message: its kind, the
    -- emoji of an emoji reaction, the key of the message it targets in
    -- message_key form, and whether it takes a reaction back. NULL on a message
    -- that is not a reaction, and on every source that stores none.
    NULL::text AS reaction_kind,
    NULL::text AS reaction_emoji,
    NULL::text AS reaction_target_key,
    NULL::boolean AS reaction_removal
FROM comms_message cm
JOIN venue v
  ON v.source = cm.source
 AND v.source_container_id = cm.thread_id
 AND v.kind IN ('dm', 'group_chat', 'email_thread')
JOIN node n ON n.id = v.node_id AND n.deleted_at IS NULL
LEFT JOIN live_contact lc ON lc.id = cm.matched_contact_id
WHERE cm.deleted_at IS NULL

UNION ALL

SELECT
    v.node_id,
    v.kind,
    'telegram:' || tm.telegram_chat_id::text || ':' || tm.telegram_message_id::text,
    tm.sent_at,
    tm.is_outgoing,
    -- A private chat's peer fields name its other side in both directions; an
    -- outgoing group message carries none.
    CASE WHEN NOT tm.is_outgoing
        THEN 'telegram:' || COALESCE(tm.peer_user_id::text, NULLIF(tm.peer_username, ''), tm.peer_phone, '')
        ELSE COALESCE('telegram:' || COALESCE(tm.peer_user_id::text, NULLIF(tm.peer_username, ''), NULLIF(tm.peer_phone, '')), '')
    END,
    lc.id,
    array_remove(ARRAY[
        NULLIF(concat_ws(' ', NULLIF(tm.peer_first_name, ''), NULLIF(tm.peer_last_name, '')), ''),
        NULLIF(tm.peer_first_name, ''),
        NULLIF(tm.peer_username, ''),
        NULLIF(tm.peer_phone, '')
    ], NULL),
    ARRAY[]::text[],
    COALESCE(tm.message_text, ''),
    '{}'::jsonb,
    tm.interaction_id,
    NULL::text,
    NULL::text,
    NULL::text,
    NULL::boolean
FROM telegram_message tm
JOIN venue v
  ON v.source = 'telegram'
 AND v.source_container_id = tm.telegram_chat_id::text
 AND v.kind IN ('dm', 'group_chat')
JOIN node n ON n.id = v.node_id AND n.deleted_at IS NULL
LEFT JOIN live_contact lc ON lc.id = tm.matched_contact_id
WHERE tm.deleted_at IS NULL

UNION ALL

SELECT
    v.node_id,
    v.kind,
    'messages:' || mm.guid,
    mm.sent_at,
    mm.is_outgoing,
    -- An outgoing group message names no recipient.
    CASE WHEN NOT mm.is_outgoing
        THEN 'messages:' || COALESCE(NULLIF(mm.peer_normalized, ''), mm.peer_handle, '')
        ELSE COALESCE('messages:' || COALESCE(NULLIF(mm.peer_normalized, ''), NULLIF(mm.peer_handle, '')), '')
    END,
    lc.id,
    array_remove(ARRAY[NULLIF(mm.peer_handle, '')], NULL),
    ARRAY[]::text[],
    COALESCE(mm.text, ''),
    '{}'::jsonb,
    mm.interaction_id,
    mm.reaction_kind::text,
    mm.reaction_emoji,
    'messages:' || mm.reaction_target_key,
    mm.reaction_removal
FROM messages_message mm
JOIN venue v
  ON v.source = 'messages'
 AND v.source_container_id = mm.chat_guid
 AND v.kind IN ('dm', 'group_chat')
JOIN node n ON n.id = v.node_id AND n.deleted_at IS NULL
LEFT JOIN live_contact lc ON lc.id = mm.matched_contact_id
WHERE mm.deleted_at IS NULL

UNION ALL

-- A meeting note belongs to the meeting venue of the calendar event its session
-- is linked to, else to its session's venue (the interaction content reader's
-- attribution).
SELECT
    v.node_id,
    v.kind,
    'anarlog_sessions:' || mn.anarlog_session_id::text,
    mn.meeting_at,
    TRUE,
    '',
    NULL,
    ARRAY[]::text[],
    ARRAY[]::text[],
    concat_ws(E'\n\n', NULLIF(mn.summary, ''), NULLIF(mn.memo, '')),
    jsonb_strip_nulls(jsonb_build_object('title', to_jsonb(NULLIF(mn.title, '')))),
    NULL::uuid,
    NULL::text,
    NULL::text,
    NULL::text,
    NULL::boolean
FROM meeting_note mn
LEFT JOIN calendar_event ce
  ON mn.linked_kind = 'event' AND ce.id = mn.linked_id
JOIN venue v
  ON (ce.id IS NOT NULL
      AND v.source = 'gcal'
      AND v.kind = 'meeting'
      AND v.source_container_id =
            octet_length(ce.gcal_event_id)::text || ':' || ce.gcal_event_id || '|' ||
            octet_length(ce.gcal_calendar_id)::text || ':' || ce.gcal_calendar_id || '|' ||
            octet_length(ce.google_account_id)::text || ':' || ce.google_account_id)
  OR (ce.id IS NULL
      AND v.source = 'anarlog_sessions'
      AND v.kind = 'session'
      AND v.source_container_id = mn.anarlog_session_id::text)
JOIN node n ON n.id = v.node_id AND n.deleted_at IS NULL
WHERE mn.deleted_at IS NULL;

COMMIT;
