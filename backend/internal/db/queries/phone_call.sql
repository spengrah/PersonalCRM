-- name: UpsertPhoneCall :one
-- Insert with no-op on call_unique_id conflict. peer_normalized comes from
-- the daemon (canonicalized via HandleNormalization). matched_contact_id
-- comes from the Pi ingest service identity-match path. mac_host_id is
-- provenance — cross-host dedup is by call_unique_id (Apple's Continuity
-- propagates the same value across hosts).
--
-- ON CONFLICT does a no-op so RETURNING reliably returns the existing row,
-- enabling concurrent dual-host push to converge on a single staging row.
-- We deliberately do NOT overwrite user-visible fields on conflict.
-- CallHistoryDB rows are mostly immutable post-recording, but a missed
-- inbound call CAN flip ZHASMESSAGE from false -> true once the
-- corresponding VoicemailService row materializes (typically within a
-- few seconds of the recording). The first push lands with
-- has_voicemail=false, and the second is silently deduped by the
-- event-log (source, source_id) unique BEFORE reaching this query.
-- Net effect: a late-arriving voicemail flag is dropped — acceptable
-- for v1.5 because (a) the daemon's ~60-90s tick cadence almost always
-- observes the voicemail row in the SAME tick, and (b) the staging
-- row's audit value is unchanged. If this gap matters later, widen the
-- DO UPDATE to refresh has_voicemail / duration_seconds / answered.
INSERT INTO phone_call (
    call_unique_id, peer_handle, peer_normalized,
    service, direction, answered, has_voicemail,
    duration_seconds, started_at,
    matched_contact_id, mac_host_id
) VALUES (
    @call_unique_id, @peer_handle, @peer_normalized,
    @service, @direction, @answered, @has_voicemail,
    @duration_seconds, @started_at,
    @matched_contact_id, @mac_host_id
)
ON CONFLICT (call_unique_id) DO UPDATE SET
    -- No-op update so RETURNING returns the existing row.
    call_unique_id = EXCLUDED.call_unique_id
RETURNING *;

-- name: TestInsertPhoneCallLinked :one
-- Test-only: inserts a phone_call already linked to an interaction. Used by the
-- venue backfill test to seed a phone container row. Production code MUST NOT
-- call this (the live path sets interaction_id via MarkPhoneCallProcessed).
INSERT INTO phone_call (
    call_unique_id, peer_handle, peer_normalized, service, direction,
    duration_seconds, started_at, matched_contact_id, interaction_id
) VALUES (
    @call_unique_id, @peer_handle, @peer_normalized, @service, @direction,
    @duration_seconds, @started_at, @matched_contact_id, @interaction_id
)
RETURNING *;

-- name: GetPhoneCallByUniqueID :one
-- Lookup by call_unique_id. Returns ErrNoRows on miss.
SELECT * FROM phone_call
WHERE call_unique_id = @call_unique_id;

-- name: GetPhoneCallByID :one
-- Lookup by primary-key UUID. Used by the meeting_note resolve-link
-- handler to verify a phone_call target exists before linking. Returns
-- ErrNoRows on miss.
SELECT * FROM phone_call
WHERE id = @id;

-- name: FindPhoneCallsInWindow :many
-- Returns phone_call rows whose started_at falls inside the linkage
-- window for the meeting_note linkage handler. No deleted_at filter —
-- phone_call has no soft-delete column (see migration 055). Backed by
-- idx_phone_call_started_at from migration 056.
SELECT * FROM phone_call
WHERE started_at BETWEEN sqlc.arg('window_start') AND sqlc.arg('window_end')
ORDER BY started_at ASC;

-- name: MarkPhoneCallProcessed :exec
-- Marks the staging row as processed and links it to the resulting
-- interaction. interaction_id is NULLable: missed-inbound-no-voicemail rows
-- get processed_at set but interaction_id stays NULL forever (content-
-- delivered cadence semantics; spec §`phone_calls` source).
UPDATE phone_call
SET processed_at = NOW(),
    interaction_id = @interaction_id
WHERE id = @id;

-- name: HardDeletePhoneCallsByMacHost :exec
-- Test-only helper. Cleanup needs hard-delete because the upsert does not
-- support a soft-delete escape hatch (phone_call has no deleted_at column —
-- staging rows have no aggregator-driven lifecycle). Scoped by mac_host_id
-- so tests can pass a fresh mac_host UUID per run and clean only their own
-- rows.
DELETE FROM phone_call
WHERE mac_host_id = @mac_host_id;

-- name: HardDeletePhoneCallByUniqueID :exec
-- Test-only helper: deletes a single phone_call row by call_unique_id.
-- Used by migration round-trip tests to clear the staging row before the
-- down migration's row-bearing guard runs.
DELETE FROM phone_call
WHERE call_unique_id = @call_unique_id;

-- name: ListPhoneCallsByInteractionIDs :many
SELECT * FROM phone_call
WHERE interaction_id = ANY(@interaction_ids::uuid[]);
