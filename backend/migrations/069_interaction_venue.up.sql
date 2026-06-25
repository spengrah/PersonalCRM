-- Venue model on interaction (graph foundation).
--
-- Adds interaction.venue_id (the shared-container node an interaction happened
-- in) and backfills it for every existing interaction whose source has a
-- resolvable container. A venue is one node per real container, keyed on the
-- venue (source, kind, source_container_id) unique from migration 064.
--
-- Wrapped in an explicit transaction: the postgres migration driver does NOT
-- auto-wrap a migration file, so this DDL + multi-statement backfill self-wraps
-- to stay all-or-nothing on a mid-file failure (Postgres DDL is transactional).
-- The whole file is idempotent (re-runnable): ADD COLUMN/INDEX IF NOT EXISTS;
-- the venue node id is a deterministic uuid_generate_v5 of (source, kind,
-- container) so a re-run computes the same id and ON CONFLICT DO NOTHING on both
-- node (PK) and venue (unique) makes it a no-op with no orphan node; the
-- venue_id UPDATEs are scoped to WHERE venue_id IS NULL.
--
-- The deterministic node id is the SAME function the live
-- ResolveVenueForInteraction helper uses (Go uuid.NewSHA1 over the same
-- namespace + name), so the backfill and the live recorders converge on one
-- venue node per container; an integration test asserts they agree.
--
-- Per-source container key:
--   email/gchat        -> comms_message.thread_id        (via interaction_id FK)
--   messages           -> messages_message.chat_guid     (via interaction_id FK)
--   telegram           -> telegram_message.telegram_chat_id (via interaction_id FK)
--   gcal               -> length-prefixed (gcal_event_id || gcal_calendar_id ||
--                         google_account_id) 3-tuple, joined by
--                         interaction.source_ref = calendar_event.id::text
--   phone_calls        -> phone_call.call_unique_id       (via interaction_id FK)
--   anarlog_sessions   -> meeting_note.anarlog_session_id, extracted from
--                         interaction.source_ref ('anarlog:<sid>:<cid>'); reuses
--                         the linked gcal meeting venue when linked to an event
--   manual / todoist   -> no container; venue_id stays NULL
--
-- gcal is ordered BEFORE anarlog so the anarlog->gcal venue reuse finds the
-- meeting venue that gcal just created.

BEGIN;

ALTER TABLE interaction ADD COLUMN IF NOT EXISTS venue_id UUID REFERENCES node(id);

CREATE INDEX IF NOT EXISTS idx_interaction_venue_id
    ON interaction (venue_id)
    WHERE venue_id IS NOT NULL AND deleted_at IS NULL;

-- venue_node_id(source, kind, container) -> the deterministic venue node id.
-- uuid_generate_v5 over a fixed venue namespace and the length-prefixed
-- (source, kind, container) name. STRICT IMMUTABLE so it can drive set-based
-- backfill SELECTs and a NULL component yields NULL (never a bogus id). The
-- namespace is an arbitrary fixed UUID dedicated to venue identity.
CREATE FUNCTION venue_node_id(p_source TEXT, p_kind TEXT, p_container TEXT)
RETURNS UUID
LANGUAGE sql
IMMUTABLE
STRICT
AS $$
    SELECT uuid_generate_v5(
        'a4f7c0e2-1b3d-4c6a-9e8f-2d5b7a1c0e94'::uuid,
        octet_length(p_source)::text || ':' || p_source || '|' ||
        octet_length(p_kind)::text || ':' || p_kind || '|' ||
        octet_length(p_container)::text || ':' || p_container
    );
$$;

-- ============================================================================
-- email + gchat -> email_thread / group_chat, keyed on comms_message.thread_id
-- ============================================================================
-- comms_message carries the interaction_id FK and the thread_id container key.
-- email rows are email threads; gchat rows are group chats (space/chat scope).
-- A NULL/empty thread_id has no shared container, so those rows are skipped.
WITH containers AS (
    SELECT DISTINCT
        cm.source,
        CASE WHEN cm.source = 'email' THEN 'email_thread' ELSE 'group_chat' END AS kind,
        cm.thread_id AS container_id
    FROM comms_message cm
    JOIN interaction i ON i.id = cm.interaction_id
    WHERE i.venue_id IS NULL AND i.deleted_at IS NULL
      AND cm.thread_id IS NOT NULL AND cm.thread_id <> ''
      AND cm.source IN ('email', 'gchat')
), ins_node AS (
    INSERT INTO node (id, type, canonical_label)
    SELECT venue_node_id(source, kind, container_id), 'venue', ''
    FROM containers
    ON CONFLICT (id) DO NOTHING
    RETURNING id
)
INSERT INTO venue (node_id, kind, source, source_container_id, title)
SELECT venue_node_id(source, kind, container_id), kind, source, container_id, NULL
FROM containers
ON CONFLICT (source, kind, source_container_id) DO NOTHING;

UPDATE interaction i
SET venue_id = v.node_id
FROM comms_message cm
JOIN venue v
  ON v.source = cm.source
 AND v.kind = CASE WHEN cm.source = 'email' THEN 'email_thread' ELSE 'group_chat' END
 AND v.source_container_id = cm.thread_id
WHERE cm.interaction_id = i.id
  AND i.venue_id IS NULL AND i.deleted_at IS NULL
  AND cm.thread_id IS NOT NULL AND cm.thread_id <> ''
  AND cm.source IN ('email', 'gchat');

-- ============================================================================
-- messages -> dm / group_chat, keyed on messages_message.chat_guid
-- ============================================================================
WITH containers AS (
    SELECT DISTINCT
        mm.chat_guid AS container_id,
        CASE WHEN mm.is_group_chat THEN 'group_chat' ELSE 'dm' END AS kind
    FROM messages_message mm
    JOIN interaction i ON i.id = mm.interaction_id
    WHERE i.venue_id IS NULL AND i.deleted_at IS NULL
      AND mm.chat_guid IS NOT NULL AND mm.chat_guid <> ''
), ins_node AS (
    INSERT INTO node (id, type, canonical_label)
    SELECT venue_node_id('messages', kind, container_id), 'venue', ''
    FROM containers
    ON CONFLICT (id) DO NOTHING
    RETURNING id
)
INSERT INTO venue (node_id, kind, source, source_container_id, title)
SELECT venue_node_id('messages', kind, container_id), kind, 'messages', container_id, NULL
FROM containers
ON CONFLICT (source, kind, source_container_id) DO NOTHING;

UPDATE interaction i
SET venue_id = v.node_id
FROM messages_message mm
JOIN venue v
  ON v.source = 'messages'
 AND v.kind = CASE WHEN mm.is_group_chat THEN 'group_chat' ELSE 'dm' END
 AND v.source_container_id = mm.chat_guid
WHERE mm.interaction_id = i.id
  AND i.venue_id IS NULL AND i.deleted_at IS NULL
  AND mm.chat_guid IS NOT NULL AND mm.chat_guid <> '';

-- ============================================================================
-- telegram -> dm / group_chat, keyed on telegram_message.telegram_chat_id
-- ============================================================================
-- chat_type 'private' is a DM; 'group'/'supergroup' are group chats. The
-- chat id is a BIGINT, stored as text in the venue container key.
WITH containers AS (
    SELECT DISTINCT
        tm.telegram_chat_id::text AS container_id,
        CASE WHEN tm.chat_type = 'private' THEN 'dm' ELSE 'group_chat' END AS kind,
        NULLIF(MAX(tm.chat_title), '') AS title
    FROM telegram_message tm
    JOIN interaction i ON i.id = tm.interaction_id
    WHERE i.venue_id IS NULL AND i.deleted_at IS NULL
    GROUP BY tm.telegram_chat_id::text,
             CASE WHEN tm.chat_type = 'private' THEN 'dm' ELSE 'group_chat' END
), ins_node AS (
    INSERT INTO node (id, type, canonical_label)
    SELECT venue_node_id('telegram', kind, container_id), 'venue', ''
    FROM containers
    ON CONFLICT (id) DO NOTHING
    RETURNING id
)
INSERT INTO venue (node_id, kind, source, source_container_id, title)
SELECT venue_node_id('telegram', kind, container_id), kind, 'telegram', container_id, title
FROM containers
ON CONFLICT (source, kind, source_container_id) DO NOTHING;

UPDATE interaction i
SET venue_id = v.node_id
FROM telegram_message tm
JOIN venue v
  ON v.source = 'telegram'
 AND v.kind = CASE WHEN tm.chat_type = 'private' THEN 'dm' ELSE 'group_chat' END
 AND v.source_container_id = tm.telegram_chat_id::text
WHERE tm.interaction_id = i.id
  AND i.venue_id IS NULL AND i.deleted_at IS NULL;

-- ============================================================================
-- phone_calls -> call, keyed on phone_call.call_unique_id
-- ============================================================================
WITH containers AS (
    SELECT DISTINCT pc.call_unique_id AS container_id
    FROM phone_call pc
    JOIN interaction i ON i.id = pc.interaction_id
    WHERE i.venue_id IS NULL AND i.deleted_at IS NULL
), ins_node AS (
    INSERT INTO node (id, type, canonical_label)
    SELECT venue_node_id('phone_calls', 'call', container_id), 'venue', ''
    FROM containers
    ON CONFLICT (id) DO NOTHING
    RETURNING id
)
INSERT INTO venue (node_id, kind, source, source_container_id, title)
SELECT venue_node_id('phone_calls', 'call', container_id), 'call', 'phone_calls', container_id, NULL
FROM containers
ON CONFLICT (source, kind, source_container_id) DO NOTHING;

UPDATE interaction i
SET venue_id = v.node_id
FROM phone_call pc
JOIN venue v
  ON v.source = 'phone_calls'
 AND v.kind = 'call'
 AND v.source_container_id = pc.call_unique_id
WHERE pc.interaction_id = i.id
  AND i.venue_id IS NULL AND i.deleted_at IS NULL;

-- ============================================================================
-- gcal -> meeting, keyed on the length-prefixed 3-tuple
-- (gcal_event_id || gcal_calendar_id || google_account_id)
-- ============================================================================
-- The gcal interaction's source_ref is the INTERNAL calendar_event.id UUID
-- (the forward path writes calendar_event.ID, not gcal_event_id). The container
-- key is the calendar_event's real uniqueness — the 3-tuple — length-prefixed
-- so a delimiter inside any component cannot alias a different tuple.
-- Ordered BEFORE anarlog so the meeting venue exists for the reuse step.
WITH containers AS (
    SELECT DISTINCT
        octet_length(ce.gcal_event_id)::text || ':' || ce.gcal_event_id || '|' ||
        octet_length(ce.gcal_calendar_id)::text || ':' || ce.gcal_calendar_id || '|' ||
        octet_length(ce.google_account_id)::text || ':' || ce.google_account_id AS container_id,
        NULLIF(MAX(ce.title), '') AS title
    FROM calendar_event ce
    JOIN interaction i ON i.source = 'gcal' AND i.source_ref = ce.id::text
    WHERE i.venue_id IS NULL AND i.deleted_at IS NULL
    GROUP BY
        octet_length(ce.gcal_event_id)::text || ':' || ce.gcal_event_id || '|' ||
        octet_length(ce.gcal_calendar_id)::text || ':' || ce.gcal_calendar_id || '|' ||
        octet_length(ce.google_account_id)::text || ':' || ce.google_account_id
), ins_node AS (
    INSERT INTO node (id, type, canonical_label)
    SELECT venue_node_id('gcal', 'meeting', container_id), 'venue', ''
    FROM containers
    ON CONFLICT (id) DO NOTHING
    RETURNING id
)
INSERT INTO venue (node_id, kind, source, source_container_id, title)
SELECT venue_node_id('gcal', 'meeting', container_id), 'meeting', 'gcal', container_id, title
FROM containers
ON CONFLICT (source, kind, source_container_id) DO NOTHING;

UPDATE interaction i
SET venue_id = v.node_id
FROM calendar_event ce
JOIN venue v
  ON v.source = 'gcal'
 AND v.kind = 'meeting'
 AND v.source_container_id =
        octet_length(ce.gcal_event_id)::text || ':' || ce.gcal_event_id || '|' ||
        octet_length(ce.gcal_calendar_id)::text || ':' || ce.gcal_calendar_id || '|' ||
        octet_length(ce.google_account_id)::text || ':' || ce.google_account_id
WHERE i.source = 'gcal'
  AND i.source_ref = ce.id::text
  AND i.venue_id IS NULL AND i.deleted_at IS NULL;

-- ============================================================================
-- anarlog_sessions -> session, keyed on meeting_note.anarlog_session_id
-- (or REUSE the linked gcal meeting venue)
-- ============================================================================
-- The anarlog interaction's source_ref is 'anarlog:<sessionID>:<contactID>', so
-- the session id is split_part(source_ref, ':', 2). Step 1 reuses the gcal
-- meeting venue for sessions linked to an event (linked_kind='event'); step 2
-- mints a session venue for everything else.

-- Step 1: REUSE the linked gcal meeting venue (the only cross-source merge).
UPDATE interaction i
SET venue_id = ce_venue.node_id
FROM meeting_note mn
JOIN calendar_event ce ON ce.id = mn.linked_id
JOIN venue ce_venue
  ON ce_venue.source = 'gcal'
 AND ce_venue.kind = 'meeting'
 AND ce_venue.source_container_id =
        octet_length(ce.gcal_event_id)::text || ':' || ce.gcal_event_id || '|' ||
        octet_length(ce.gcal_calendar_id)::text || ':' || ce.gcal_calendar_id || '|' ||
        octet_length(ce.google_account_id)::text || ':' || ce.google_account_id
WHERE i.source = 'anarlog_sessions'
  AND i.venue_id IS NULL AND i.deleted_at IS NULL
  AND i.source_ref IS NOT NULL
  AND mn.anarlog_session_id = split_part(i.source_ref, ':', 2)::uuid
  AND mn.deleted_at IS NULL
  AND mn.linked_kind = 'event';

-- Step 2: mint a session venue for the remaining (unlinked) anarlog sessions.
WITH containers AS (
    SELECT DISTINCT split_part(i.source_ref, ':', 2) AS container_id
    FROM interaction i
    WHERE i.source = 'anarlog_sessions'
      AND i.venue_id IS NULL AND i.deleted_at IS NULL
      AND i.source_ref IS NOT NULL
      AND split_part(i.source_ref, ':', 2) <> ''
), ins_node AS (
    INSERT INTO node (id, type, canonical_label)
    SELECT venue_node_id('anarlog_sessions', 'session', container_id), 'venue', ''
    FROM containers
    ON CONFLICT (id) DO NOTHING
    RETURNING id
)
INSERT INTO venue (node_id, kind, source, source_container_id, title)
SELECT venue_node_id('anarlog_sessions', 'session', container_id), 'session', 'anarlog_sessions', container_id, NULL
FROM containers
ON CONFLICT (source, kind, source_container_id) DO NOTHING;

UPDATE interaction i
SET venue_id = v.node_id
FROM venue v
WHERE v.source = 'anarlog_sessions'
  AND v.kind = 'session'
  AND v.source_container_id = split_part(i.source_ref, ':', 2)
  AND i.source = 'anarlog_sessions'
  AND i.venue_id IS NULL AND i.deleted_at IS NULL
  AND i.source_ref IS NOT NULL
  AND split_part(i.source_ref, ':', 2) <> '';

DROP FUNCTION venue_node_id(TEXT, TEXT, TEXT);

COMMIT;
