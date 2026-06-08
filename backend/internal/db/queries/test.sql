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

-- name: GetRiverJobStateByID :one
-- Test assertion — returns the river_job.state for a single job id. Used by
-- rescue-on-crash polling to wait out River's async completer
-- (running->completed lands after the worker returns). River exposes no
-- production sqlc layer; this is the test-boundary seam, mirroring the
-- existing CountRiverJobsByKind test query.
SELECT state FROM river_job WHERE id = @id;

-- name: SweepRiverJobsInCloneForTest :execrows
-- Test setup — drop ALL river_job rows, but ONLY when connected to a
-- per-package clone DB (current_database() matching the clone-name prefix).
-- The rescue-on-crash test calls this to clear foreign-kind leftovers (e.g.
-- interaction_recorder) created by earlier tests in the same per-package
-- clone, which its live River client would otherwise fetch and churn on,
-- widening the completer race. Fail-closed by construction: on a shared test
-- DB or the dev DB the WHERE is false and it deletes nothing, so a manual
-- no-tag run pointed at a real database can never wipe its queue. The prefix
-- mirrors clonePrefix in internal/testdb (cannot import that build-tagged
-- package from an untagged test file). EVERY underscore in the prefix is
-- escaped so each is matched literally, not as a LIKE single-char wildcard —
-- for a broad delete the guard must match the exact clone-name prefix, not a
-- looser pattern.
DELETE FROM river_job
WHERE current_database() LIKE 'personal\_crm\_test\_clone\_%' ESCAPE '\';

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

-- ============================================================================
-- Synthetic seed toolkit (internal/synthetic) test-only support queries.
-- All synthetic-package DB access routes through these sqlc bindings (via
-- repository/synthetic_support.go) so the package never inlines raw SQL.
-- ============================================================================

-- name: SyntheticCountUnfinalizedRiverJobsForEventsByContacts :one
-- Settle Gate B (part 1): counts unfinalized river_job rows whose target
-- event (args->>'event_id') was causally produced by one of this replay's
-- contacts. Every contact-bearing event payload carries a scalar
-- payload->>'contact_id' (interaction.recorded, calendar.attended/declined,
-- email.received/sent, message.*, task.*, contact_methods.added, ...), so a
-- single contact_id projection covers all cascade kinds generically — not a
-- fixed kind list. Scoped to this replay's contacts (NOT a global kind count)
-- so concurrent unrelated jobs on the shared test DB never block the gate.
SELECT COUNT(*) FROM river_job
WHERE finalized_at IS NULL
  AND (args->>'event_id') IN (
    SELECT id::text FROM event
    WHERE (payload->>'contact_id') = ANY(@contact_ids::text[])
  );

-- name: SyntheticCountUnfinalizedMessagingAggregateJobs :one
-- Settle Gate B (part 2): the messaging_aggregate_for_contact job keys on
-- (contact_id, source) in its args, NOT event_id, so it is invisible to the
-- event-scoped Gate B query above. Counts unfinalized aggregate jobs for this
-- replay's contacts + source (mirrors CountRematchDispatcherJobsByContact).
SELECT COUNT(*) FROM river_job
WHERE finalized_at IS NULL
  AND kind = 'messaging_aggregate_for_contact'
  AND (args->>'source') = @source::text
  AND (args->>'contact_id') = ANY(@contact_ids::text[]);

-- name: SyntheticListEventIdsForContacts :many
-- Cleanup event-id capture (part 1): every event.id whose payload references
-- one of this replay's contacts. Covers the full non-prefixed cascade
-- (interaction.recorded uses interaction.ID as source_id, calendar.attended
-- uses an internal ref, etc.) generically via payload->>'contact_id'.
SELECT id FROM event
WHERE (payload->>'contact_id') = ANY(@contact_ids::text[]);

-- name: SyntheticListEventIdsBySourceAndSourceIdPrefix :many
-- Cleanup event-id capture (part 2): adapter-direct root events that carry NO
-- CRM contact id (raw_message.* / external_contact.upserted roots, and
-- unknown/pending replays that touch no seeded contact). Keyed by the
-- synthetic (source, source_id-prefix) the adapter wrote. The UNION of this
-- and SyntheticListEventIdsForContacts is the cleanup event set — leaving a
-- root event behind would make a later same-namespace replay dedup on the
-- (source, source_id) unique and skip inline ingest (idempotency break).
-- Caller passes a BARE prefix; the '%' is appended here (matches the existing
-- Delete*ByPrefix conventions).
SELECT id FROM event
WHERE source = @source::text
  AND source_id LIKE @source_id_prefix || '%';

-- name: SyntheticDeleteEventConsumerClaimsByEventIds :execrows
-- Cleanup step 1: claims for this replay's events (by tracked event id).
DELETE FROM event_consumer_claim WHERE event_id = ANY(@event_ids::uuid[]);

-- name: SyntheticDeleteInteractionsByIds :execrows
-- Cleanup step 2: interactions by tracked id.
DELETE FROM interaction WHERE id = ANY(@interaction_ids::uuid[]);

-- name: SyntheticDeleteEventsByIds :execrows
-- Cleanup step 3: events by tracked id (NOT by source — that would wipe
-- other tests' rows sharing the source value on the shared DB).
DELETE FROM event WHERE id = ANY(@event_ids::uuid[]);

-- name: SyntheticDeleteCommsMessagesByExternalIdPrefix :execrows
-- Cleanup step 4: comms_message rows whose external_id is ns-prefixed.
-- Caller passes a BARE prefix; '%' is appended here.
DELETE FROM comms_message WHERE external_id LIKE @external_id_prefix || '%';

-- name: SyntheticDeleteMessagesMessageByGuidPrefix :execrows
-- Cleanup step 5: messages_message rows whose guid is ns-prefixed.
-- Caller passes a BARE prefix; '%' is appended here.
DELETE FROM messages_message WHERE guid LIKE @guid_prefix || '%';

-- name: SyntheticDeleteExternalIdentitiesByIdentifierPrefix :execrows
-- Cleanup step 8 (primary): identities whose normalized identifier is
-- ns-prefixed. MatchOrCreate for GCal attendee / external_contact email
-- matching creates identities with source_id NULL keyed by the synthetic
-- IDENTIFIER (e.g. 'synth-<ns>-...@synthetic.example'), which a source_id-prefix
-- delete MISSES. Deleting by the identifier prefix catches both the
-- source_id-NULL and source_id-set synthetic email/handle identities BEFORE the
-- contact delete (external_identity survives contact delete via ON DELETE SET
-- NULL, so it would otherwise pollute future matching). Caller passes a BARE
-- prefix; '%' is appended here. (Phone identities use the shared 555-01xx
-- fictional range and are not ns-prefixed; they carry no contact link after the
-- contact delete and re-match cleanly, so they are intentionally left.)
DELETE FROM external_identity WHERE identifier LIKE @identifier_prefix || '%';

-- name: SyntheticDeleteExternalIdentitiesBySourceIdPrefix :execrows
-- Cleanup step 8 (backstop): catches any ns-prefixed source_id rows the
-- identifier-prefix delete missed. Caller passes a BARE prefix; '%' appended.
DELETE FROM external_identity WHERE source_id LIKE @source_id_prefix || '%';

-- name: SyntheticDeleteContactTasksByContactIds :execrows
-- Cleanup step 10: contact_task has no deleted_at; hard delete by contact.
DELETE FROM contact_task WHERE contact_id = ANY(@contact_ids::uuid[]);

-- name: SyntheticListContactTaskIdsByProvider :many
-- Todoist replay: snapshot the set of contact_task ids for a provider so the
-- replay can diff before/after its (globally-scoped) reconcile and track the
-- rows it created — even for cadence-bearing contacts it did not seed — so
-- cleanup removes exactly those rows and never strands a task on an unrelated
-- contact in the shared test DB.
SELECT id FROM contact_task WHERE provider = @provider;

-- name: SyntheticDeleteContactTasksByIds :execrows
-- Todoist replay cleanup: delete the contact_task rows the replay's reconcile
-- created (the before/after diff from SyntheticListContactTaskIdsByProvider).
DELETE FROM contact_task WHERE id = ANY(@task_ids::uuid[]);

-- name: SyntheticDeleteContactMethodsByContactIds :execrows
-- Cleanup step 11: contact_method by contact.
DELETE FROM contact_method WHERE contact_id = ANY(@contact_ids::uuid[]);

-- name: SyntheticDeleteNotesByContactIds :execrows
-- Cleanup step 12: note by contact.
DELETE FROM note WHERE contact_id = ANY(@contact_ids::uuid[]);

-- name: SyntheticDeleteContactsByIds :execrows
-- Cleanup step 13: contact by tracked id. A true DELETE (not soft) so
-- ON DELETE CASCADE fires for contact_enrichment (and any cascade FK).
DELETE FROM contact WHERE id = ANY(@contact_ids::uuid[]);

-- name: SyntheticDeleteMacHostById :execrows
-- Cleanup step 14: the seeded revoked synthetic mac_host by id.
DELETE FROM mac_host WHERE id = @id;

-- name: SyntheticCountContactsByIds :one
-- Cleanup assertion — count surviving contact rows for the given ids.
SELECT COUNT(*) FROM contact WHERE id = ANY(@contact_ids::uuid[]);

-- name: SyntheticCountTelegramMessagesInPeerBand :one
-- Harness setup collision detection (D5): count live telegram_message rows whose
-- peer_user_id falls in this namespace's reserved sub-block [band_start,
-- band_end). A non-zero count means another namespace already occupies the band
-- (probabilistic collision), so NewHarness re-salts the namespace or fails loudly
-- rather than risking a cross-namespace cleanup wipe.
SELECT COUNT(*) FROM telegram_message
WHERE peer_user_id >= @band_start
  AND peer_user_id < @band_end
  AND deleted_at IS NULL;

-- name: SyntheticCountStrandedTelegramMessagesByPeer :one
-- Settle Gate A (telegram unknown-sender): a message row exists for the peer
-- with matched_contact_id IS NULL (the stranded/discovery-candidate state).
SELECT COUNT(*) FROM telegram_message
WHERE peer_user_id = @peer_user_id
  AND matched_contact_id IS NULL
  AND deleted_at IS NULL;

-- Settle Gate A (seeded sender) — per-source-message linkage. Keyed on THIS
-- replay's exact synthetic source id, so it is exact-to-this-replay (a prior
-- same-contact interaction can't satisfy it early) AND idempotent (a re-replay
-- of the same payload leaves the row linked, so the predicate stays true).

-- name: SyntheticCountLinkedCommsMessageByExternalId :one
-- gmail/gchat: the comms_message row for (source, external_id) has an
-- interaction_id (the derived interaction landed).
SELECT COUNT(*) FROM comms_message
WHERE source = @source
  AND external_id = @external_id
  AND interaction_id IS NOT NULL
  AND deleted_at IS NULL;

-- name: SyntheticCountLinkedMessagesMessageByGuid :one
-- iMessage: the staging row for the guid has an interaction_id.
SELECT COUNT(*) FROM messages_message
WHERE guid = @guid
  AND interaction_id IS NOT NULL
  AND deleted_at IS NULL;

-- name: SyntheticCountLinkedTelegramMessageByMessageId :one
-- telegram: the message row for the telegram_message_id has an interaction_id.
SELECT COUNT(*) FROM telegram_message
WHERE telegram_message_id = @telegram_message_id
  AND interaction_id IS NOT NULL
  AND deleted_at IS NULL;

-- name: SyntheticCountProcessedCalendarEventByGcalId :one
-- gcal seeded: the calendar_event for the gcal id has the contact in
-- matched_contact_ids AND last_contacted_updated=true (the attended interaction
-- published). Idempotent: a re-replay leaves the row processed.
SELECT COUNT(*) FROM calendar_event
WHERE gcal_event_id = @gcal_event_id
  AND @contact_id::uuid = ANY(matched_contact_ids)
  AND last_contacted_updated = true;

-- name: SyntheticCountStrandedMessagesMessageByGuid :one
-- Settle Gate A (iMessage unknown-sender): the staging row for the guid landed
-- (processed) with matched_contact_id IS NULL.
SELECT COUNT(*) FROM messages_message
WHERE guid = @guid
  AND matched_contact_id IS NULL
  AND deleted_at IS NULL;

-- name: SyntheticCountUnmatchedExternalContactBySourceId :one
-- Settle Gate A (Mac-contact unknown-sender): the external_contact row for the
-- entity id exists with match_status='unmatched'.
SELECT COUNT(*) FROM external_contact
WHERE source_id = @source_id
  AND match_status = 'unmatched'
  AND deleted_at IS NULL;

-- name: SyntheticCountMatchedExternalContactBySourceId :one
-- Settle Gate A (Mac-contact seeded): the external_contact row for the entity
-- id exists linked to a CRM contact (match_status='matched').
SELECT COUNT(*) FROM external_contact
WHERE source_id = @source_id
  AND match_status = 'matched'
  AND crm_contact_id IS NOT NULL
  AND deleted_at IS NULL;

-- name: SyntheticCountUnmatchedCalendarEventByGcalId :one
-- Settle Gate A (GCal unknown-attendee): the calendar_event for the gcal id
-- exists with an empty matched_contact_ids array. calendar_event has no
-- deleted_at column (hard-delete table), so no soft-delete filter.
SELECT COUNT(*) FROM calendar_event
WHERE gcal_event_id = @gcal_event_id
  AND matched_contact_ids = '{}';
