-- Test data management queries
-- These queries are used by the test API endpoints to seed and cleanup test data

-- name: DeleteContactsByNamePrefix :execrows
DELETE FROM contact WHERE full_name LIKE $1 || '%';

-- name: DeleteExternalContactsByDisplayNamePrefix :execrows
DELETE FROM external_contact WHERE display_name LIKE $1 || '%';

-- name: DeleteExternalContactsBySourceIDPrefix :execrows
DELETE FROM external_contact WHERE source_id LIKE $1 || '%';

-- name: DeleteExternalContactsBySourceForTest :execrows
-- Test teardown — hard-deletes ALL external_contact rows for a given
-- source string. The known-IDs integration tests use this when they
-- seed rows under a synthetic source value and need a targeted
-- cleanup that ignores soft-delete state. Production code must never
-- call this; it bypasses the tombstone contract and the
-- crm_contact_id/match_status preservation rules.
DELETE FROM external_contact WHERE source = @source;

-- name: CountContactsByNamePrefix :one
SELECT COUNT(*) FROM contact WHERE full_name LIKE $1 || '%';

-- name: CountExternalContactsByDisplayNamePrefix :one
SELECT COUNT(*) FROM external_contact
WHERE display_name LIKE $1 || '%'
  AND deleted_at IS NULL;

-- name: DeleteCalendarEventsByTitlePrefix :execrows
DELETE FROM calendar_event WHERE title LIKE $1 || '%';

-- name: DeleteCalendarEventsByGcalEventIdPrefix :execrows
DELETE FROM calendar_event WHERE gcal_event_id LIKE $1 || '%';

-- name: DeleteTelegramMessagesByPeerUserID :execrows
DELETE FROM telegram_message WHERE peer_user_id = $1;

-- name: DeleteTelegramMessagesByMessageIDs :execrows
DELETE FROM telegram_message WHERE telegram_message_id = ANY($1::int[]);

-- name: DeleteExternalIdentitiesBySourceID :execrows
DELETE FROM external_identity WHERE source_id = $1;

-- Mac host test helpers — per .ai/rules/core.md rule 2 (no raw SQL in
-- Go test fixtures), test setup uses these instead of pool.Exec.

-- name: SeedMacHost :one
-- Inserts a host with caller-supplied hostname + bcrypted api_key_hash.
-- Used by integration tests that need to bypass the pairing flow.
-- RETURNING is the minimal id-only set so step-by-step migration tests
-- that run this against earlier schemas (before columns added in later
-- migrations exist) don't trip on the column expansion of RETURNING *.
INSERT INTO mac_host (hostname, daemon_version, protocol_version, api_key_hash)
VALUES (@hostname, @daemon_version, @protocol_version, @api_key_hash)
RETURNING id;

-- name: SeedRevokedMacHost :one
-- Inserts a host that is already revoked (api_key_revoked_at = NOW()).
-- Used by integration tests that only need a valid mac_host UUID as an
-- FK target (e.g., messages_message.mac_host_id) and do NOT care about
-- pairing or auth state. The singleton index idx_mac_host_singleton only
-- applies to rows WHERE api_key_revoked_at IS NULL, so this helper can
-- be called freely from parallel test packages without contending with
-- the pairing-flow tests that hold the singleton slot.
INSERT INTO mac_host (hostname, daemon_version, protocol_version, api_key_hash, api_key_revoked_at)
VALUES (@hostname, @daemon_version, @protocol_version, @api_key_hash, NOW())
RETURNING *;

-- name: SeedPairingToken :one
-- Inserts a pairing token with caller-supplied hash + expiry. Tests use
-- this to seed expired tokens (cannot mint via the real Create path
-- because the service enforces a forward-only TTL).
INSERT INTO mac_host_pairing_token (token_hash, expires_at)
VALUES (@token_hash, @expires_at)
RETURNING *;

-- name: SeedExternalSyncState :one
-- Seeds an external_sync_state row at caller-supplied next_sync_at.
-- Used by scheduler-exclusion tests to plant a push-strategy row whose
-- next_sync_at is due, then assert ListDueAccounts skips it.
INSERT INTO external_sync_state
    (source, account_id, enabled, status, strategy, next_sync_at)
VALUES (@source, @account_id, @enabled, @status, @strategy, @next_sync_at)
RETURNING *;

-- name: DeleteAllMacHosts :execrows
-- Test teardown — hard delete so the singleton index is empty for the
-- next test. mac_host has no deleted_at column, so soft-delete is not
-- an option.
--
-- Excludes rows whose hostname starts with 'test-host-' (the
-- messages_message_repository_test fixture prefix). Those rows are
-- pre-revoked stand-ins used purely as FK targets by a parallel test
-- package; wiping them mid-run breaks
-- messages_message.mac_host_id FK inserts because Go runs test
-- packages in parallel against the shared test DB.
DELETE FROM mac_host WHERE hostname NOT LIKE 'test-host-%';

-- name: DeleteAllPairingTokens :execrows
DELETE FROM mac_host_pairing_token;

-- name: DeleteExternalIdentitiesBySource :execrows
-- Test teardown — drop external_identity rows seeded by a test under
-- a known source string (e.g., 'messages'). Used in raw_message ingest
-- integration tests to ensure shared-DB cleanup is complete between
-- runs.
DELETE FROM external_identity WHERE source = @source;

-- name: DeleteEventsBySource :execrows
-- Test teardown — drop event rows by source. Mirrors
-- DeleteExternalIdentitiesBySource for the event log.
DELETE FROM event WHERE source = @source;

-- name: DeleteRiverJobsByKindAny :execrows
-- Test teardown — drop river_job rows whose kind is in the given
-- array. Scoped to test-emitted kinds so we don't wipe production-
-- shape rows on a shared DB. River doesn't expose a sqlc layer; this
-- is the operator-test seam.
DELETE FROM river_job WHERE kind = ANY(@kinds::text[]);

-- name: CountMessagesMessageByGuid :one
-- Test assertion — count rows with the given guid (typically 0 or 1
-- under the partial unique index). Used by duplicate-detection tests.
SELECT COUNT(*) FROM messages_message WHERE guid = @guid;

-- name: CountRiverJobsByKindUnfinalized :one
-- Test assertion — count unfinalized River jobs of the given kind.
-- Used to verify ingest enqueues the expected number of aggregator
-- jobs. River's own admin SQL is OK to query at the test boundary;
-- production code never reads river_job directly.
SELECT COUNT(*) FROM river_job WHERE kind = @kind AND finalized_at IS NULL;

-- name: CountRiverJobsByKind :one
-- Test assertion — count ALL River jobs of the given kind (including
-- finalized). When the test runs against a River client with active
-- workers, jobs can be picked up and finalized between insert and
-- assertion; counting by kind alone is timing-resilient. Cross-test
-- pollution is bounded by DeleteRiverJobsByKindAny in test cleanup.
SELECT COUNT(*) FROM river_job WHERE kind = @kind;

-- name: DeleteInteractionsByContactAndSource :execrows
-- Test teardown — drops interactions seeded by a test under the given
-- (contact_id, source) pair. Scoped to the seeded contact so production
-- data is never wiped.
DELETE FROM interaction WHERE contact_id = @contact_id AND source = @source;

-- name: CountInteractionsByIDContactAndSource :one
-- Test assertion — confirms exactly the expected interaction row exists
-- for the (id, contact_id, source) tuple. Used by the raw_message
-- end-to-end test to verify Stage 3 created the row the staging
-- table's interaction_id points to.
SELECT COUNT(*) FROM interaction
WHERE id = @id AND contact_id = @contact_id AND source = @source;

-- name: GetInteractionSourceCheckDef :one
-- Test assertion — returns the rendered definition of the
-- interaction_source_check CHECK constraint via pg_get_constraintdef.
-- Scoped by (conrelid, conname) because constraint names are NOT
-- globally unique — scoping to the interaction relation avoids a future
-- cross-table/-schema name collision. The descriptor-vs-CHECK agreement
-- test parses the returned ARRAY[...] string literals to assert the
-- live CHECK set equals the repository.InteractionSource* constants.
-- Read-only catalog access; production code never calls this.
SELECT pg_get_constraintdef(c.oid)::text AS constraint_def
FROM pg_constraint c
WHERE c.conrelid = 'interaction'::regclass
  AND c.conname = 'interaction_source_check';
