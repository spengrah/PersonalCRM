-- Meeting Note queries
-- Spec: .ai/spec/mac-daemon-phase-2-anarlog-matching.md

-- name: InsertMeetingNote :one
-- Inserts a meeting_note staging row. linkage_state must be supplied by the
-- caller (the ingest tx computes it from the linkage detection algorithm);
-- no DB-level default exists. input_hash / resolved_set_hash /
-- last_content_hash / meeting_at are supplied verbatim per the re-sync
-- diff algorithm (see service/ingest.go handleMeetingNoteRecorded).
--
-- ON CONFLICT DO NOTHING handles the concurrent first-insert race against
-- the partial unique index idx_meeting_note_session_id WHERE deleted_at IS
-- NULL. When two concurrent batches try to first-insert the same session
-- UUID, one wins and the other receives zero rows back; the caller then
-- re-reads with FOR UPDATE and falls through to the update path. sqlc :one
-- returns ErrNoRows in that case, which the repository translates to
-- db.ErrNotFound.
INSERT INTO meeting_note (
    anarlog_session_id,
    title,
    summary,
    memo,
    participants,
    mac_host_id,
    linked_kind,
    linked_id,
    linkage_state,
    input_hash,
    resolved_set_hash,
    last_content_hash,
    meeting_at
) VALUES (
    sqlc.arg('anarlog_session_id'),
    sqlc.arg('title'),
    sqlc.arg('summary'),
    sqlc.arg('memo'),
    sqlc.arg('participants'),
    sqlc.arg('mac_host_id'),
    sqlc.arg('linked_kind'),
    sqlc.arg('linked_id'),
    sqlc.arg('linkage_state'),
    sqlc.arg('input_hash'),
    sqlc.arg('resolved_set_hash'),
    sqlc.arg('last_content_hash'),
    sqlc.arg('meeting_at')
)
ON CONFLICT (anarlog_session_id) WHERE deleted_at IS NULL DO NOTHING
RETURNING *;

-- name: GetMeetingNoteBySessionID :one
-- Returns the live (non-soft-deleted) meeting_note row for a given anarlog
-- session UUID. Used by the ingest path for dedup and re-sync detection.
SELECT * FROM meeting_note
WHERE anarlog_session_id = sqlc.arg('anarlog_session_id')
  AND deleted_at IS NULL;

-- name: GetMeetingNoteBySessionIDForUpdate :one
-- Tombstone-aware lookup that holds a row-level lock for the duration of
-- the caller's tx. Used by the meeting_note.recorded inline handler so a
-- concurrent re-sync for the same session UUID serializes behind the
-- first writer (see service/ingest.go handleMeetingNoteRecorded step 2).
-- Returns tombstoned rows too — the revive path inspects DeletedAt to
-- decide between revive and re-link branches.
SELECT * FROM meeting_note
WHERE anarlog_session_id = sqlc.arg('anarlog_session_id')
FOR UPDATE;

-- name: GetTombstonedMeetingNoteBySessionID :one
-- Probe used by the dispatch loop's revive-bypass: when a duplicate
-- meeting_note.recorded event arrives (same source_id, content unchanged)
-- and a tombstoned row exists, the inline handler still runs so it can
-- revive instead of silently leaving the row tombstoned. Filters
-- explicitly to tombstoned rows because the live partial-unique index
-- already covers the live-row case.
SELECT * FROM meeting_note
WHERE anarlog_session_id = sqlc.arg('anarlog_session_id')
  AND deleted_at IS NOT NULL
LIMIT 1;

-- name: UpdateMeetingNoteOnResync :one
-- Single update query used for both carry-forward and re-link branches
-- of the re-sync algorithm. Caller controls the values: carry-forward
-- passes the prior linkage values, re-link passes the recomputed values.
-- Avoids two near-identical queries.
--
-- The deleted_at filter prevents stomping on a tombstoned row (revive
-- runs its own UPDATE path that also clears deleted_at).
UPDATE meeting_note SET
    title             = sqlc.arg('title'),
    summary           = sqlc.arg('summary'),
    memo              = sqlc.arg('memo'),
    participants      = sqlc.arg('participants'),
    linked_kind       = sqlc.arg('linked_kind'),
    linked_id         = sqlc.arg('linked_id'),
    linkage_state     = sqlc.arg('linkage_state'),
    input_hash        = sqlc.arg('input_hash'),
    resolved_set_hash = sqlc.arg('resolved_set_hash'),
    last_content_hash = sqlc.arg('last_content_hash'),
    meeting_at        = sqlc.arg('meeting_at')
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING *;

-- name: ReviveMeetingNote :one
-- Clears deleted_at + writes the full updatable column set in a single
-- statement so a tombstoned row that re-receives meeting_note.recorded
-- comes back live with the new content. Defensive WHERE deleted_at IS
-- NOT NULL keeps it idempotent across concurrent revive races.
UPDATE meeting_note SET
    deleted_at        = NULL,
    title             = sqlc.arg('title'),
    summary           = sqlc.arg('summary'),
    memo              = sqlc.arg('memo'),
    participants      = sqlc.arg('participants'),
    linked_kind       = sqlc.arg('linked_kind'),
    linked_id         = sqlc.arg('linked_id'),
    linkage_state     = sqlc.arg('linkage_state'),
    input_hash        = sqlc.arg('input_hash'),
    resolved_set_hash = sqlc.arg('resolved_set_hash'),
    last_content_hash = sqlc.arg('last_content_hash'),
    meeting_at        = sqlc.arg('meeting_at')
WHERE id = sqlc.arg('id') AND deleted_at IS NOT NULL
RETURNING *;

-- name: SoftDeleteMeetingNoteBySessionID :exec
-- Tombstones the live row for a session UUID. last_content_hash,
-- input_hash, meeting_at are preserved on the row so a future revive
-- has the prior content snapshot available. Idempotent (no-op when no
-- live row exists).
UPDATE meeting_note SET deleted_at = NOW()
WHERE anarlog_session_id = sqlc.arg('anarlog_session_id') AND deleted_at IS NULL;

-- name: ListKnownMeetingNoteIDsByHost :many
-- Returns (source_id, last_content_hash) for every live meeting_note
-- row owned by the given mac_host. Powers GET
-- /api/v1/host/:id/sync/anarlog_sessions/known-ids. Tombstoned rows are
-- excluded — the daemon's set-diff reconciliation requires that rows
-- the Pi has soft-deleted are NOT reported as known.
SELECT anarlog_session_id::text AS source_id, last_content_hash
FROM meeting_note
WHERE mac_host_id = sqlc.arg('mac_host_id') AND deleted_at IS NULL
ORDER BY source_id;

-- name: TestHardDeleteMeetingNotesBySessionIDPrefix :exec
-- TEST ONLY. Hard-deletes meeting_note rows whose session UUID (as text)
-- starts with the given prefix. Used by t.Cleanup to remove fixtures
-- inserted by a test. Covers both live and tombstoned rows.
DELETE FROM meeting_note
WHERE anarlog_session_id::text LIKE sqlc.arg('session_id_prefix')::text;

-- name: TestHardDeleteMeetingNotesByHostID :exec
-- TEST ONLY. Hard-deletes every meeting_note row owned by the given
-- mac_host. Used by t.Cleanup when the test seeds meeting_notes with
-- system-generated UUIDs (no exploitable prefix). Covers both live and
-- tombstoned rows.
DELETE FROM meeting_note
WHERE mac_host_id = sqlc.arg('mac_host_id');
