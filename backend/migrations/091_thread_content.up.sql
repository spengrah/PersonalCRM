-- One definition of a thread's content: every content row of every live venue,
-- one row per stored copy, with the source identity of its sender.
--
-- The thread-context builder (internal/threadctx) reads a thread's context, its
-- latest content time and the threads that gained content in a window from this
-- one view, so the three reads agree by construction.
--
-- Columns:
--   venue_id             the thread (venue node id); only live venues appear.
--   venue_kind           dm, group_chat, email_thread, meeting or session.
--   message_key          the source message itself, equal for every per-contact
--                        copy of it (comms_message stores one row per message and
--                        matched contact).
--   sent_at              the content's own time (a meeting note's meeting_at).
--   from_user            the user wrote it: an outbound message or a meeting note.
--   sender_key           the sender's source identity, prefixed by source, for a
--                        message the user did not write; never a display name.
--                        Empty for the user's own content.
--   peer_contact_id      the live contact the copy is attributed to: the sender of
--                        a received message, a recipient of the user's own.
--   sender_aliases       the names and handles the source gives the sender.
--   body                 the text.
--   headers              email address headers and subject, a meeting note's title.
CREATE VIEW thread_content AS
SELECT
    v.node_id AS venue_id,
    v.kind AS venue_kind,
    (cm.source || ':' || cm.external_id)::text AS message_key,
    cm.sent_at,
    cm.direction = 'outbound' AS from_user,
    (CASE WHEN cm.direction = 'inbound'
        THEN cm.source || ':' || COALESCE(NULLIF(cm.peer_normalized, ''), cm.peer_handle, '')
        ELSE ''
    END)::text AS sender_key,
    lc.id AS peer_contact_id,
    array_remove(ARRAY[
        NULLIF(cm.source_metadata->>'from_name', ''),
        NULLIF(cm.source_metadata->>'push_name', ''),
        NULLIF(cm.source_metadata->>'from', ''),
        -- An inbound row's peer is its sender; an outbound gchat row's peer is
        -- the user's own address. Every other outbound peer is a recipient.
        CASE WHEN cm.direction = 'inbound' OR cm.source = 'gchat' THEN NULLIF(cm.peer_handle, '') END
    ], NULL)::text[] AS sender_aliases,
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
    )) AS headers
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
    CASE WHEN NOT tm.is_outgoing
        THEN 'telegram:' || COALESCE(tm.peer_user_id::text, NULLIF(tm.peer_username, ''), tm.peer_phone, '')
        ELSE ''
    END,
    lc.id,
    -- An outgoing row's peer fields name the chat's other side, not the user.
    CASE WHEN tm.is_outgoing THEN ARRAY[]::text[] ELSE array_remove(ARRAY[
        NULLIF(concat_ws(' ', NULLIF(tm.peer_first_name, ''), NULLIF(tm.peer_last_name, '')), ''),
        NULLIF(tm.peer_first_name, ''),
        NULLIF(tm.peer_username, ''),
        NULLIF(tm.peer_phone, '')
    ], NULL) END,
    COALESCE(tm.message_text, ''),
    '{}'::jsonb
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
    CASE WHEN NOT mm.is_outgoing
        THEN 'messages:' || COALESCE(NULLIF(mm.peer_normalized, ''), mm.peer_handle, '')
        ELSE ''
    END,
    lc.id,
    CASE WHEN mm.is_outgoing THEN ARRAY[]::text[] ELSE array_remove(ARRAY[NULLIF(mm.peer_handle, '')], NULL) END,
    COALESCE(mm.text, ''),
    '{}'::jsonb
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
    concat_ws(E'\n\n', NULLIF(mn.summary, ''), NULLIF(mn.memo, '')),
    jsonb_strip_nulls(jsonb_build_object('title', to_jsonb(NULLIF(mn.title, ''))))
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
