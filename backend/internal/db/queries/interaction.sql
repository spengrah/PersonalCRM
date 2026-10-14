-- Interaction queries

-- name: GetInteraction :one
SELECT * FROM interaction WHERE id = $1 AND deleted_at IS NULL;

-- name: ListContactInteractions :many
SELECT * FROM interaction
WHERE contact_id = $1 AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT $2 OFFSET $3;

-- name: CreateInteraction :one
INSERT INTO interaction (contact_id, source, source_ref, occurred_at, description, direction, venue_id, interaction_node_id)
VALUES (
    sqlc.arg('contact_id'),
    sqlc.arg('source'),
    sqlc.narg('source_ref'),
    sqlc.arg('occurred_at'),
    sqlc.narg('description'),
    COALESCE(sqlc.narg('direction'), 'mutual'),
    sqlc.narg('venue_id'),
    sqlc.narg('interaction_node_id')
)
RETURNING *;

-- name: ResolveInteractionNode :one
-- The observation step's resolution: finds or creates the interaction node of
-- an observation by the per-source identity rule (resolve_interaction_node in
-- migration 087, the one implementation). A burst source returns no row (no node).
SELECT resolve_interaction_node(sqlc.arg('source')::text, sqlc.narg('source_ref')::text) AS node_id;

-- name: AssignInteractionNode :exec
-- Gives a row that has no interaction node its node, when a source observes it
-- again. The IS NULL guard makes an assigned node permanent: this is the only
-- statement besides the create insert that writes interaction_node_id.
UPDATE interaction
SET interaction_node_id = sqlc.arg('interaction_node_id')
WHERE id = sqlc.arg('id')
  AND interaction_node_id IS NULL;

-- name: UpdateInteractionDirection :one
-- Promote an outbound interaction to mutual when a reply arrives (in-place update)
UPDATE interaction
SET direction = sqlc.arg(direction),
    occurred_at = sqlc.arg(occurred_at)
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteInteraction :exec
UPDATE interaction SET deleted_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountContactInteractions :one
SELECT COUNT(*) FROM interaction
WHERE contact_id = $1 AND deleted_at IS NULL;

-- name: FindInteractionBySourceRef :one
-- Find an existing interaction by contact, source, and source_ref (for deduplication)
SELECT * FROM interaction
WHERE contact_id = $1 AND source = $2 AND source_ref = $3 AND deleted_at IS NULL
LIMIT 1;

-- name: TestGetInteractionNode :one
-- Test-only read of an interaction node's subtype row, for backfill fixtures
-- that check a node's source and persisted identity key.
SELECT * FROM interaction_node
WHERE node_id = sqlc.arg(node_id);

-- name: TestGetInteractionIncludingDeleted :one
-- Test-only read for migration fixtures that verify a deleted interaction is
-- outside a historical repair selector. Production reads remain live-only.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = sqlc.arg(source)
  AND source_ref = sqlc.arg(source_ref)
LIMIT 1;

-- name: FindInteractionInWindow :one
-- Find an existing manual interaction within a time window for a given
-- direction (for manual deduplication). Direction is part of the dedup
-- key so a user logging outbound then inbound for the same contact
-- within the window correctly produces two separate rows.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = sqlc.arg(source)
  AND direction = sqlc.arg(direction)
  AND deleted_at IS NULL
  AND occurred_at BETWEEN sqlc.arg(window_start) AND sqlc.arg(window_end)
ORDER BY occurred_at DESC
LIMIT 1;

-- name: FindRecentOutboundTelegramInteraction :one
-- Find the most recent outbound telegram interaction for a contact in a specific chat
-- within a time window. source_ref_prefix should include trailing % for LIKE match.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = 'telegram'
  AND direction = 'outbound'
  AND source_ref LIKE sqlc.arg(source_ref_prefix)
  AND occurred_at >= sqlc.arg(window_start)
  AND occurred_at <= sqlc.arg(window_end)
  AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT 1;

-- name: FindRecentTelegramInteraction :one
-- Find the most recent telegram interaction for a contact in a specific chat
-- with a given direction. Used for incremental coalescing.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = 'telegram'
  AND direction = sqlc.arg(direction)
  AND source_ref LIKE sqlc.arg(source_ref_prefix)
  AND occurred_at >= sqlc.arg(window_start)
  AND occurred_at <= sqlc.arg(window_end)
  AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT 1;

-- name: FindRecentInteractionBySourceAndDirection :one
-- Source-neutral generalization of FindRecentTelegramInteraction.
-- Used by the shared aggregator (backend/internal/messaging/aggregation)
-- for same-direction coalescing. source_ref_prefix should include
-- trailing % for LIKE match.
--
-- The ESCAPE clause lets adapters whose source-ref segments may
-- contain `_` or `%` (e.g., Apple Messages chat.guid values like
-- "iMessage;-;_chat-uuid_") supply their own escape character `\`
-- without false-matching unrelated rows. Numeric-only chat IDs
-- (Telegram) pass through unchanged since they never contain LIKE
-- wildcards.
--
-- The explicit ::text cast on the bind variable keeps sqlc inferring
-- pgtype.Text for the parameter; without it the ESCAPE clause causes
-- sqlc to fall back to []byte (Postgres treats the LIKE pattern as
-- bytea-compatible in that form). Plain LIKE-on-text behavior is what
-- the existing callers expect.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = sqlc.arg(source)
  AND direction = sqlc.arg(direction)
  AND source_ref LIKE sqlc.arg(source_ref_prefix)::text ESCAPE '\'
  AND occurred_at >= sqlc.arg(window_start)
  AND occurred_at <= sqlc.arg(window_end)
  AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT 1;

-- name: FindRecentOutboundInteractionBySource :one
-- Source-neutral generalization of FindRecentOutboundTelegramInteraction.
-- Used by the shared aggregator for time-based reply bridging on inbound
-- sessions.
--
-- ESCAPE clause: same rationale as FindRecentInteractionBySourceAndDirection.
-- See that query for the ::text cast rationale.
SELECT * FROM interaction
WHERE contact_id = sqlc.arg(contact_id)
  AND source = sqlc.arg(source)
  AND direction = 'outbound'
  AND source_ref LIKE sqlc.arg(source_ref_prefix)::text ESCAPE '\'
  AND occurred_at >= sqlc.arg(window_start)
  AND occurred_at <= sqlc.arg(window_end)
  AND deleted_at IS NULL
ORDER BY occurred_at DESC
LIMIT 1;

-- name: HardDeleteInteractionsBySourceRefPrefix :exec
-- Test-only: hard-deletes interactions whose source matches and source_ref
-- begins with prefix. Used by integration tests to purge per-run rows
-- cleanly; soft-delete is unsafe because the (source, source_ref) partial
-- unique constraint would block a same-source_ref re-insert on the next
-- test run even with deleted_at set.
DELETE FROM interaction
WHERE source = sqlc.arg(source)
  AND source_ref LIKE sqlc.arg(source_ref_prefix);

-- name: UpdateInteractionVenue :exec
-- Sets venue_id on an existing interaction. Used by the anarlog re-sync path so
-- a retained interaction whose session was re-linked (e.g. session -> gcal event)
-- moves to the correct venue node. Does not touch cadence columns.
UPDATE interaction
SET venue_id = sqlc.narg('venue_id')
WHERE id = sqlc.arg('id')
  AND deleted_at IS NULL;

-- name: UpdateInteractionTimestamp :one
-- Extend an existing interaction's occurred_at and description (incremental coalescing)
UPDATE interaction
SET occurred_at = sqlc.arg(occurred_at),
    description = sqlc.arg(description)
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL
RETURNING *;

-- name: HasResponseAfter :one
-- Returns TRUE if any later inbound/mutual effective participation exists for
-- the contact after the given outreach time. Used by the FollowUpManager's
-- out-of-order guard: an outbound event arriving after a response has
-- already landed must not produce a stale follow-up.
-- Reads effective participation (migration 089), so a response a correction
-- removed is no response, and a merged participation answers as one.
SELECT EXISTS (
    SELECT 1 FROM effective_participation
    WHERE contact_id = sqlc.arg('contact_id')
      AND direction IN ('inbound', 'mutual')
      AND occurred_at > sqlc.arg('outreach_at')::timestamptz
    LIMIT 1
) AS has_response;

-- name: ListSessionAttributedInteractions :many
-- Returns all live interactions attributed to a specific anarlog session
-- (both impromptu / orphan-with-tags entries and walk-in supplementals).
-- Used by the re-sync diff path in the meeting_note.recorded inline
-- handler to compute the (existing - desired) set that needs
-- soft-deleting.
SELECT * FROM interaction
WHERE source = 'anarlog_sessions'
  AND source_ref LIKE sqlc.arg('source_ref_prefix')
  AND deleted_at IS NULL
ORDER BY source_ref;

-- name: TestInsertInteraction :one
-- Test-only: inserts an interaction with a caller-supplied id, source, and
-- source_ref and a NULL venue_id, bypassing the recorder. Used by the venue
-- backfill migration test to stand up pre-existing (venue-less) interactions
-- that the 069 backfill then populates. The caller supplies the interaction
-- node, which the repository wrapper resolves through the observation step.
-- Production code MUST NOT call this.
INSERT INTO interaction (id, contact_id, source, source_ref, occurred_at, direction, interaction_node_id)
VALUES (
    sqlc.arg('id'),
    sqlc.arg('contact_id'),
    sqlc.arg('source'),
    sqlc.narg('source_ref'),
    sqlc.arg('occurred_at'),
    sqlc.arg('direction'),
    sqlc.narg('interaction_node_id')
)
RETURNING *;

-- name: AcquireSourceRefAggregateLock :exec
-- Takes a transaction-scoped advisory lock keyed on an interaction
-- aggregation source_ref. Used by the email-interaction consumer to
-- serialize all jobs for the same (contact, thread, local-day)
-- aggregation key, so the read-compute-write of the forward-only
-- occurred_at guard is atomic per key. The lock auto-releases on
-- commit/rollback. hashtextextended folds the source_ref string into the
-- bigint advisory-lock key space; a rare hash collision only
-- over-serializes two unrelated keys (a perf cost), never
-- under-serializes (a correctness cost). Mirrors the per-account
-- sync-enqueue lock in external_sync.sql.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg('lock_key')::text, 0));

-- name: ListContactTimelineFiltered :many
-- One page of a contact's interaction history, as (kind, id) keys.
-- Two kinds share the time axis: 'interaction' (a live interaction row) and
-- 'missed_call' (an unanswered inbound call that left no voicemail, which
-- ingest stores as a phone_call with no interaction; see migration 055). The
-- kinds argument selects which kinds the page holds. A missed call has no
-- content container, so any venue filter excludes it.
SELECT item.kind, item.id FROM (
  SELECT 'interaction'::text AS kind, interaction.id, interaction.occurred_at FROM interaction
  WHERE 'interaction' = ANY(sqlc.arg('kinds')::text[])
    AND interaction.contact_id = sqlc.arg('contact_id')
    AND interaction.deleted_at IS NULL
    AND (sqlc.narg('from_time')::timestamptz IS NULL OR interaction.occurred_at >= sqlc.narg('from_time'))
    AND (sqlc.narg('to_time')::timestamptz IS NULL OR interaction.occurred_at < sqlc.narg('to_time'))
    AND (
      (sqlc.narg('venue_source')::text IS NULL AND sqlc.narg('venue_container')::text IS NULL)
      OR (
        sqlc.narg('venue_source')::text IS NOT NULL AND sqlc.narg('venue_container')::text IS NOT NULL
        AND CASE
          WHEN sqlc.narg('venue_source')::text IN ('email', 'gchat', 'whatsapp') THEN EXISTS (
            SELECT 1 FROM comms_message cm WHERE cm.interaction_id = interaction.id AND cm.deleted_at IS NULL
              AND cm.source = sqlc.narg('venue_source')::text AND cm.thread_id = sqlc.narg('venue_container')::text)
          WHEN sqlc.narg('venue_source')::text = 'telegram' THEN EXISTS (
            SELECT 1 FROM telegram_message tm WHERE tm.interaction_id = interaction.id AND tm.deleted_at IS NULL
              AND tm.telegram_chat_id::text = sqlc.narg('venue_container')::text)
          WHEN sqlc.narg('venue_source')::text = 'messages' THEN EXISTS (
            SELECT 1 FROM messages_message mm WHERE mm.interaction_id = interaction.id AND mm.deleted_at IS NULL
              AND mm.chat_guid = sqlc.narg('venue_container')::text)
          ELSE FALSE
        END
      )
    )
  UNION ALL
  SELECT 'missed_call'::text AS kind, phone_call.id, phone_call.started_at AS occurred_at FROM phone_call
  WHERE 'missed_call' = ANY(sqlc.arg('kinds')::text[])
    AND phone_call.matched_contact_id = sqlc.arg('contact_id')
    AND phone_call.interaction_id IS NULL
    AND phone_call.direction = 'inbound'
    AND phone_call.answered IS NOT TRUE
    AND NOT phone_call.has_voicemail
    AND sqlc.narg('venue_source')::text IS NULL AND sqlc.narg('venue_container')::text IS NULL
    AND (sqlc.narg('from_time')::timestamptz IS NULL OR phone_call.started_at >= sqlc.narg('from_time'))
    AND (sqlc.narg('to_time')::timestamptz IS NULL OR phone_call.started_at < sqlc.narg('to_time'))
) AS item
ORDER BY item.occurred_at DESC, item.id DESC
LIMIT sqlc.arg('limit_count') OFFSET sqlc.arg('offset_count');

-- name: CountContactTimelineFiltered :one
-- Counts the items ListContactTimelineFiltered pages over, under the same
-- arguments.
SELECT COUNT(*) FROM (
  SELECT interaction.id FROM interaction
  WHERE 'interaction' = ANY(sqlc.arg('kinds')::text[])
    AND interaction.contact_id = sqlc.arg('contact_id')
    AND interaction.deleted_at IS NULL
    AND (sqlc.narg('from_time')::timestamptz IS NULL OR interaction.occurred_at >= sqlc.narg('from_time'))
    AND (sqlc.narg('to_time')::timestamptz IS NULL OR interaction.occurred_at < sqlc.narg('to_time'))
    AND (
      (sqlc.narg('venue_source')::text IS NULL AND sqlc.narg('venue_container')::text IS NULL)
      OR (
        sqlc.narg('venue_source')::text IS NOT NULL AND sqlc.narg('venue_container')::text IS NOT NULL
        AND CASE
          WHEN sqlc.narg('venue_source')::text IN ('email', 'gchat', 'whatsapp') THEN EXISTS (
            SELECT 1 FROM comms_message cm WHERE cm.interaction_id = interaction.id AND cm.deleted_at IS NULL
              AND cm.source = sqlc.narg('venue_source')::text AND cm.thread_id = sqlc.narg('venue_container')::text)
          WHEN sqlc.narg('venue_source')::text = 'telegram' THEN EXISTS (
            SELECT 1 FROM telegram_message tm WHERE tm.interaction_id = interaction.id AND tm.deleted_at IS NULL
              AND tm.telegram_chat_id::text = sqlc.narg('venue_container')::text)
          WHEN sqlc.narg('venue_source')::text = 'messages' THEN EXISTS (
            SELECT 1 FROM messages_message mm WHERE mm.interaction_id = interaction.id AND mm.deleted_at IS NULL
              AND mm.chat_guid = sqlc.narg('venue_container')::text)
          ELSE FALSE
        END
      )
    )
  UNION ALL
  SELECT phone_call.id FROM phone_call
  WHERE 'missed_call' = ANY(sqlc.arg('kinds')::text[])
    AND phone_call.matched_contact_id = sqlc.arg('contact_id')
    AND phone_call.interaction_id IS NULL
    AND phone_call.direction = 'inbound'
    AND phone_call.answered IS NOT TRUE
    AND NOT phone_call.has_voicemail
    AND sqlc.narg('venue_source')::text IS NULL AND sqlc.narg('venue_container')::text IS NULL
    AND (sqlc.narg('from_time')::timestamptz IS NULL OR phone_call.started_at >= sqlc.narg('from_time'))
    AND (sqlc.narg('to_time')::timestamptz IS NULL OR phone_call.started_at < sqlc.narg('to_time'))
) AS item;


-- Effective participation reads (arc seam S4). Each reads the one derivation,
-- the effective_participation and interaction_observation_verdict views of
-- migration 089; none derives participation of its own.

-- name: ListEffectiveParticipationForContact :many
-- One row per effective participation of the contact, latest first.
SELECT * FROM effective_participation ep
WHERE ep.contact_id = sqlc.arg('contact_id')
ORDER BY ep.occurred_at DESC, ep.members->0->>'observation_id';

-- name: ListInteractionParticipants :many
-- The effective participants of the interaction whose alias tree holds
-- node_id (the root or any node merged into it), each with its entry for the
-- root. Only the contacts observed on the tree are derived.
SELECT * FROM effective_participation ep
WHERE ep.interaction_node_id = interaction_node_root(sqlc.arg('node_id')::uuid)
  AND ep.contact_id = ANY(ARRAY(
      SELECT i.contact_id FROM interaction i
      WHERE i.interaction_node_id IN (
          SELECT interaction_alias_tree(interaction_node_root(sqlc.arg('node_id')::uuid)))
        AND i.deleted_at IS NULL))
ORDER BY ep.contact_id;

-- name: GetObservationVerdict :one
-- Whether a correction excludes the live observation, with the contact and
-- root its effective entry is keyed by.
SELECT
    v.contact_id,
    v.root_node_id,
    v.excluded
FROM interaction_observation_verdict v
WHERE v.observation_id = sqlc.arg('observation_id');

-- name: GetEffectiveParticipationOfObservation :one
-- The contact's effective entry that holds the observation as a member. The
-- root qual (NULL for a message burst) narrows the derivation to that entry.
SELECT * FROM effective_participation ep
WHERE ep.contact_id = sqlc.arg('contact_id')
  AND ep.interaction_node_id IS NOT DISTINCT FROM sqlc.narg('root_node_id')::uuid
  AND ep.members @> jsonb_build_array(jsonb_build_object('observation_id', sqlc.arg('observation_id')::uuid));

-- name: ListInteractionAliasTree :many
-- Every node of the alias tree that holds node_id, each with its own alias
-- (NULL for the root). No row when node_id names no interaction node.
SELECT n.id, n.merged_into
FROM node n
WHERE n.type = 'interaction'
  AND n.id IN (
      SELECT interaction_alias_tree(interaction_node_root(sqlc.arg('node_id')::uuid)))
  AND EXISTS (
      SELECT 1 FROM node q WHERE q.id = sqlc.arg('node_id')::uuid AND q.type = 'interaction')
ORDER BY n.id;

-- name: ListLiveContactsObservedOnNodes :many
-- The live contacts with a live observation on any of the interaction nodes,
-- whether or not a correction excludes it: the contacts a merge of those
-- nodes recomputes.
SELECT DISTINCT i.contact_id
FROM interaction i
JOIN live_contact c ON c.id = i.contact_id
WHERE i.interaction_node_id = ANY(sqlc.arg('node_ids')::uuid[])
  AND i.deleted_at IS NULL
ORDER BY i.contact_id;

-- name: InteractionRecordingSuppressedFollowUp :one
-- Whether the recording of the observation suppressed its follow-up (a send
-- task's completion, CAD-012): read from the interaction.recorded event that
-- recording published, which is keyed by the observation's id under its
-- source. An observation recorded without that event suppressed nothing.
SELECT EXISTS (
    SELECT 1
    FROM interaction i
    JOIN event e ON e.source = i.source AND e.source_id = i.id::text
    WHERE i.id = @id
      AND e.kind = 'interaction.recorded'
      AND COALESCE((e.payload->>'suppress_follow_up')::boolean, false)
)::boolean AS suppressed;

-- name: ListInteractionsByIDs :many
-- Live interactions with the given ids, in no particular order. Hydrates the
-- interaction keys of a ListContactTimelineFiltered page.
SELECT * FROM interaction
WHERE id = ANY(@ids::uuid[]) AND deleted_at IS NULL;
