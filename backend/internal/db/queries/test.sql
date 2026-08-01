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

-- name: SyntheticCountEventConsumerClaimsByEventIds :one
-- Cleanup assertion — surviving claims for a set of event ids. event_consumer_claim
-- has NO fk to event, so deleting an event does not take its claims with it: a
-- sweep that dropped the events while its claim delete failed would leave rows
-- that no later cleanup can even name (the event ids are derived FROM the events).
-- The ids must therefore be captured before the sweep and asserted against after.
SELECT COUNT(*) FROM event_consumer_claim WHERE event_id = ANY(@event_ids::uuid[]);

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
-- Cleanup step 8: identities whose normalized identifier shares an ns-scoped
-- prefix. MatchOrCreate for GCal attendee / external_contact email matching
-- creates identities with source_id NULL keyed by the synthetic IDENTIFIER (e.g.
-- 'synth-<ns>-...@synthetic.example'), which a source_id-prefix delete MISSES.
-- Deleting by the identifier prefix catches both the source_id-NULL and
-- source_id-set synthetic identities BEFORE the contact delete (external_identity
-- survives contact delete via ON DELETE SET NULL, so it would otherwise pollute
-- future matching). Called once with the 'synth-<ns>-' string prefix (email/
-- handle identities) and once with the namespace's normalized phone-digit prefix
-- ('+1<area>55501...') — synthetic phones are now ns-scoped via the per-namespace
-- area code (factory.phoneFor), so phone identities no longer leak. Caller passes
-- a BARE prefix; '%' appended.
DELETE FROM external_identity WHERE identifier LIKE @identifier_prefix || '%';

-- name: SyntheticDeleteExternalIdentitiesBySourceIdPrefix :execrows
-- Cleanup step 8 (backstop): catches any ns-prefixed source_id rows the
-- identifier-prefix delete missed. Caller passes a BARE prefix; '%' appended.
DELETE FROM external_identity WHERE source_id LIKE @source_id_prefix || '%';

-- name: SyntheticSelectLinkedContactIdsByExternalContactIds :many
-- Cleanup id-set: the CRM contacts the product linked to this namespace's own
-- import candidates. It is a third recovery route for contacts, unioned with the
-- name prefix and the ownership records rather than replacing either.
--
-- It exists because resolving a candidate CREATES a contact whose name the
-- product derives from the candidate, not from the namespace: an anarlog_title
-- import takes the writer's title-cased token ('Synth-<ns>-…') and a handle-only
-- telegram import takes the '@handle' — and LIKE is case-sensitive, so neither
-- matches the lower-case 'synth-<ns>-' prefix. The harness never saw those
-- contacts, so no ownership record exists either. crm_contact_id is the FK the
-- product itself writes, which is what makes them findable at all.
SELECT DISTINCT crm_contact_id FROM external_contact
WHERE id = ANY(@external_contact_ids::uuid[]) AND crm_contact_id IS NOT NULL;

-- name: SyntheticDeleteExternalContactsByIds :execrows
-- Cleanup step 9 (ownership): import candidates by the namespace's OWNERSHIP
-- record, unioned with the source_id-prefix delete above rather than replacing
-- it. Three of the declarable candidate sources have a production source_id that
-- carries no namespace-prefixed string — telegram keys on a decimal peer id and
-- anarlog_title on a SHA-256 (token || session) digest — so the prefix sweep has
-- nothing to match for them and this is the only delete that reaches those rows.
-- A hard DELETE, like every other cleanup step.
DELETE FROM external_contact WHERE id = ANY(@external_contact_ids::uuid[]);

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

-- name: SyntheticCountExternalIdentitiesByIdentifierPrefix :one
-- Harness setup collision detection (D5): count external_identity rows whose
-- normalized identifier shares the namespace's phone-digit prefix. Non-zero means
-- another namespace already occupies this phone sub-block, so NewHarness re-salts.
-- Caller passes a BARE prefix; '%' is appended here.
SELECT COUNT(*) FROM external_identity
WHERE identifier LIKE @identifier_prefix || '%';

-- name: SyntheticCountContactMethodsByValueNormalizedPrefix :one
-- Harness setup collision detection (D5) — PRIMARY phone-band check. A seeded
-- synthetic contact's phone lives ONLY as a contact_method (no external_identity
-- until a later replay), and identity matching cross-matches via
-- contact_method.value_normalized, so this is where the cross-namespace phone
-- collision actually originates. Counts live (non-deleted-contact) contact_method
-- rows whose normalized value shares the namespace's phone prefix. Caller passes
-- a BARE prefix; '%' is appended here.
SELECT COUNT(*) FROM contact_method cm
JOIN contact c ON c.id = cm.contact_id
WHERE cm.value_normalized LIKE @value_normalized_prefix || '%'
  AND c.deleted_at IS NULL;

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
-- telegram: the message row for (peer_user_id, telegram_message_id) has an
-- interaction_id. Scoped by peer_user_id too — the peer band IS collision-checked
-- at setup (resolveNamespace), whereas the message-id bucket is narrower and not
-- checked; scoping by both means a colliding-message-id row in another namespace
-- (necessarily a different peer band) can never satisfy this predicate early.
SELECT COUNT(*) FROM telegram_message
WHERE peer_user_id = @peer_user_id
  AND telegram_message_id = @telegram_message_id
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

-- name: SyntheticCountCalendarEventByGcalId :one
-- Settle Gate A (GCal decline terminal): count ALL calendar_event rows for the
-- gcal id regardless of status/match. The cutover decline branch DELETES the
-- row, so the decline test settles on this reaching 0. calendar_event has no
-- deleted_at column (hard-delete table).
SELECT COUNT(*) FROM calendar_event
WHERE gcal_event_id = @gcal_event_id;

-- name: SyntheticCountUnmatchedExternalContactByEmailPrefix :one
-- Settle/assert (GCal unmatched-attendee import candidate): the GCal provider
-- stores an unmatched attendee as an external_contact with source='gcal_attendee'
-- and source_id = the NORMALIZED (lowercased/trimmed) attendee email, which for a
-- synthetic unknown attendee carries the 'synth-<ns>-' prefix. Counts those
-- unmatched candidates for this namespace. Caller passes a BARE prefix; '%'
-- appended here.
SELECT COUNT(*) FROM external_contact
WHERE source = 'gcal_attendee'
  AND source_id LIKE @source_id_prefix || '%'
  AND match_status = 'unmatched'
  AND deleted_at IS NULL;

-- name: SyntheticCountTelegramMessagesByChatAndMessageId :one
-- Group assertion: count telegram_message rows for (telegram_chat_id,
-- telegram_message_id). Tests assert 0 for the untracked-by-size group case (the
-- shouldTrackChat gate returns before UpsertMessage) and 1 for tracked.
SELECT COUNT(*) FROM telegram_message
WHERE telegram_chat_id = @telegram_chat_id
  AND telegram_message_id = @telegram_message_id
  AND deleted_at IS NULL;

-- name: SyntheticCountTelegramChatConfigInChatIdBand :one
-- Harness setup collision detection: count telegram_chat_config rows whose
-- telegram_chat_id falls in this namespace's reserved peer band [band_start,
-- band_end) — group chat ids are drawn from that band. A non-zero count means a
-- leftover config row occupies the band, so NewHarness re-salts.
SELECT COUNT(*) FROM telegram_chat_config
WHERE telegram_chat_id >= @band_start
  AND telegram_chat_id < @band_end;

-- name: SyntheticDeleteTelegramChatConfigsByChatIds :execrows
-- Cleanup: delete the group telegram_chat_config rows a group replay created, by
-- the exact tracked chat ids (telegram_chat_config has no namespace column —
-- keyed only by telegram_chat_id).
DELETE FROM telegram_chat_config WHERE telegram_chat_id = ANY(@chat_ids::bigint[]);

-- name: SyntheticCountTelegramBarePeerRowsInBand :one
-- Harness setup collision detection: count telegram external_contact +
-- external_identity rows keyed by a BARE peer-id source_id that falls in this
-- namespace's reserved peer band [band_start, band_end). A discovery/stranded
-- replay creates these (source='telegram', source_id=<peer id>); a crashed prior
-- run can leave them with no remaining telegram_message row, so the peer-band
-- check on telegram_message alone would miss them. A non-zero count means the
-- band is occupied → NewHarness re-salts.
--
-- The cast is wrapped in a CASE that only evaluates source_id::bigint for
-- all-digit values (bounded to 18 digits, safely under the int64 max): a bare
-- WHERE `source_id ~ '...' AND source_id::bigint >= ...` is NOT safe because
-- PostgreSQL may reorder the predicates and run the cast on a non-numeric
-- source_id (other tests create telegram rows with text source ids like
-- 'tg-discovery-upsert-*'), raising "invalid input syntax for type bigint". The
-- CASE makes the cast conditional, so non-numeric rows yield NULL and fall out of
-- the range comparison.
SELECT (
  (SELECT COUNT(*) FROM external_contact ec
   WHERE ec.source = 'telegram'
     AND (CASE WHEN ec.source_id ~ '^[0-9]{1,18}$' THEN ec.source_id::bigint END)
           >= @band_start::bigint
     AND (CASE WHEN ec.source_id ~ '^[0-9]{1,18}$' THEN ec.source_id::bigint END)
           < @band_end::bigint
     AND ec.deleted_at IS NULL)
  +
  (SELECT COUNT(*) FROM external_identity ei
   WHERE ei.source = 'telegram'
     AND (CASE WHEN ei.source_id ~ '^[0-9]{1,18}$' THEN ei.source_id::bigint END)
           >= @band_start::bigint
     AND (CASE WHEN ei.source_id ~ '^[0-9]{1,18}$' THEN ei.source_id::bigint END)
           < @band_end::bigint)
)::bigint;

-- name: SyntheticDeleteTelegramExternalContactsByPeerIds :execrows
-- Cleanup: telegram discovery candidate external_contact rows are keyed by
-- source='telegram', source_id = the BARE peer user id (not an ns-prefixed
-- string), so the ns-prefix external_contact delete misses them. A stranded /
-- unknown-sender replay that crosses the discovery threshold upserts one; delete
-- them by the exact tracked peer ids (string form).
DELETE FROM external_contact
WHERE source = 'telegram' AND source_id = ANY(@peer_ids::text[]);

-- name: SyntheticDeleteTelegramExternalIdentitiesByPeerIds :execrows
-- Cleanup: MatchPeer creates an external_identity for an unmatched telegram peer
-- keyed by source='telegram', source_id = the BARE peer user id. The synthetic
-- handle is normalized to 'synth_<ns>_<n>' (underscores), which the ns-prefix
-- ('synth-<ns>-') identifier delete does NOT match, so clear these by the exact
-- tracked peer ids before the contact delete (external_identity survives contact
-- delete via ON DELETE SET NULL and would otherwise pollute future matching).
DELETE FROM external_identity
WHERE source = 'telegram' AND source_id = ANY(@peer_ids::text[]);

-- name: SyntheticCountNodesByLabelPrefix :one
-- Graph identity test support: count nodes whose canonical_label is ns-prefixed,
-- so a test can scope assertions to its own namespace's nodes on the shared test
-- DB. Caller passes a BARE prefix; '%' is appended here.
SELECT COUNT(*) FROM node WHERE canonical_label LIKE @label_prefix || '%';

-- name: SyntheticDeleteNodesByLabelPrefix :execrows
-- Graph identity cleanup: hard-delete nodes whose canonical_label is ns-prefixed.
-- entity and venue cascade via their ON DELETE CASCADE FK to node, so this one
-- delete removes a namespace's node+entity+venue rows. Caller passes a BARE
-- prefix; '%' is appended here.
DELETE FROM node WHERE canonical_label LIKE @label_prefix || '%';

-- name: SyntheticCountAssertionsForSubject :one
-- Assertion-store test support: count the assertions whose subject is a given
-- node, so a test scopes its assertions to its own namespace's subject node on
-- the shared test DB.
SELECT COUNT(*) FROM assertion WHERE subject_node_id = $1;

-- name: SyntheticGetNodeForContact :one
-- Contact→node dual-write test support: fetch the person node a contact owns
-- (node.id == contact.id). Returns the live (non-soft-deleted) node row so a
-- test can assert the dual-write created it with the expected type/label.
SELECT * FROM node WHERE id = $1 AND deleted_at IS NULL;

-- name: SyntheticCountContactsByFullName :one
-- Contact→node dual-write test support: count contacts with an exact full_name
-- (namespace-prefixed names are unique per test), so a rollback test asserts a
-- failed-tx contact did not survive without paging the whole contact list.
SELECT COUNT(*) FROM contact WHERE full_name = $1 AND deleted_at IS NULL;

-- name: SyntheticDeleteNodesByIds :execrows
-- Cleanup: the person node a seeded contact owns (node.id == contact.id), so
-- the harness teardown removes the nodes its dual-writing SeedContact created
-- alongside the contacts. Hard delete, keyed by the tracked contact ids.
DELETE FROM node WHERE id = ANY(@node_ids::uuid[]);

-- name: SyntheticDeleteVenueNodesByIds :execrows
-- Cleanup: hard-delete the venue nodes the real recorders minted on the replay
-- path (interaction.venue_id → node), keyed by the tracked venue node ids. The
-- venue subtype row cascades via its ON DELETE CASCADE FK to node. Guarded by
-- type='venue' (defense-in-depth: never touch a person/entity node) AND by
-- NOT EXISTS any interaction still referencing it — so a venue shared with an
-- interaction this run did not clean up (e.g. a group container another
-- namespace also used) is left intact rather than raising the restrict FK.
DELETE FROM node
WHERE id = ANY(@node_ids::uuid[])
  AND type = 'venue'
  AND NOT EXISTS (SELECT 1 FROM interaction i WHERE i.venue_id = node.id);

-- name: SyntheticCountVenueNodesByIds :one
-- Cleanup assertion — count surviving venue nodes among the given ids (scoped to
-- THIS run's tracked venue node ids, so it is immune to parallel tests creating
-- their own venue nodes on the shared DB, unlike a global venue-node count).
SELECT COUNT(*) FROM node WHERE id = ANY(@node_ids::uuid[]) AND type = 'venue';

-- name: SyntheticDeleteAssertionsForNode :execrows
-- Assertion-store cleanup: hard-delete the assertions touching a node in EITHER
-- position (provenance cascades). The assertion → node FK is restrict (NO
-- ACTION), so a test MUST clear its assertions before deleting its nodes; this
-- targeted delete is the cleanup primitive for that. superseded_by is a nullable
-- self-FK, so a single multi-row DELETE clears a closed-pair set in one shot.
DELETE FROM assertion WHERE subject_node_id = $1 OR object_node_id = $1;

-- name: SyntheticDeleteRelationshipSignalsForNodes :execrows
-- Cleanup: hard-delete the relationship_signal rows a profile seeded on the given
-- subject nodes, keyed by the tracked node ids. relationship_signal.subject_node_id
-- is a real FK→node (NO ACTION, no soft delete), so these rows MUST be cleared
-- BEFORE their subject nodes — the teardown runs this before the person/entity node
-- deletes.
DELETE FROM relationship_signal WHERE subject_node_id = ANY(@node_ids::uuid[]);

-- name: SyntheticCountRelationshipSignalsForNodes :one
-- Cleanup/coverage assertion — count relationship_signal rows for the given subject
-- nodes (scoped to THIS run's tracked node ids, so it is immune to parallel tests
-- seeding their own signals on the shared DB). Used to assert ≥1 signal exists for
-- the seeded nodes (coverage) and that 0 remain after teardown.
SELECT COUNT(*) FROM relationship_signal WHERE subject_node_id = ANY(@node_ids::uuid[]);

-- name: SyntheticCountLiveNodesByIds :one
-- Merge/soft-delete coverage test support: count the live (deleted_at IS NULL) nodes
-- among the given ids, scoped to THIS run's tracked node ids. Used to assert a merge
-- winner node stays live (== id count) while soft-deleted + merge-loser nodes are
-- tombstoned (== 0).
SELECT COUNT(*) FROM node WHERE id = ANY(@node_ids::uuid[]) AND deleted_at IS NULL;

-- name: SyntheticCountSettledCommsMessagesByExternalIds :one
-- gmail/gchat batch: how many of these external ids have a comms_message row for
-- the source with an interaction_id. Shared by BOTH sources (the single-message
-- predicate SyntheticCountLinkedCommsMessageByExternalId is likewise shared) —
-- gmail and gchat differ only in the source literal the caller passes.
SELECT COUNT(DISTINCT external_id) FROM comms_message
WHERE source = @source
  AND external_id = ANY(@external_ids::text[])
  AND interaction_id IS NOT NULL
  AND deleted_at IS NULL;

-- name: SyntheticCountCommsMessagesByExternalIds :one
-- gchat batch PROGRESS probe (not a gate): how many of these external ids have a
-- comms_message row at all, linked or not. The GChat provider writes these rows
-- SYNCHRONOUSLY inside Sync, whereas the interaction linkage arrives later from a
-- River consumer — so this is the only signal that can tell "the sweep has not
-- presented this space yet" apart from "the aggregate has not run yet". The
-- bucket loop polls it between Syncs; the terminal gate stays the linked count.
SELECT COUNT(DISTINCT external_id) FROM comms_message
WHERE source = @source
  AND external_id = ANY(@external_ids::text[])
  AND deleted_at IS NULL;

-- name: SyntheticCountSettledMessagesMessagesByGuids :one
-- iMessage batch: how many of these guids have a staging row with an
-- interaction_id.
SELECT COUNT(DISTINCT guid) FROM messages_message
WHERE guid = ANY(@guids::text[])
  AND interaction_id IS NOT NULL
  AND deleted_at IS NULL;

-- name: SyntheticCountSettledTelegramMessagesByPeerAndMessageIds :one
-- telegram batch: how many of these (peer_user_id, telegram_message_id) PAIRS
-- have a message row with an interaction_id. The key is composite for the same
-- reason the single-message predicate's is (see
-- SyntheticCountLinkedTelegramMessageByMessageId): the peer band is
-- collision-checked at namespace setup, the message-id bucket is not — so a
-- message-id-only batch gate could be satisfied early, or pushed permanently
-- past the batch size, by another namespace's rows. The arrays are parallel:
-- element i of each names one payload.
SELECT COUNT(DISTINCT (tm.peer_user_id, tm.telegram_message_id))
FROM unnest(@peer_user_ids::bigint[]) WITH ORDINALITY AS p(peer_user_id, ord)
JOIN unnest(@telegram_message_ids::int[]) WITH ORDINALITY AS m(telegram_message_id, ord)
  ON p.ord = m.ord
JOIN telegram_message tm
  ON tm.peer_user_id = p.peer_user_id
 AND tm.telegram_message_id = m.telegram_message_id
WHERE tm.interaction_id IS NOT NULL
  AND tm.deleted_at IS NULL;

-- name: SyntheticCountMatchedCalendarEventsByGcalIds :one
-- gcal batch: how many of these (gcal_event_id, contact_id) PAIRS have a
-- calendar_event row carrying the contact in matched_contact_ids AND
-- last_contacted_updated = true (the attended interaction published). The pair is
-- the unit because one batch may target several contacts, and a merely-matched
-- event does not prove the past-event publish ran. The arrays are parallel:
-- element i of each names one payload.
SELECT COUNT(DISTINCT (ce.gcal_event_id, c.contact_id))
FROM unnest(@gcal_event_ids::text[]) WITH ORDINALITY AS g(gcal_event_id, ord)
JOIN unnest(@contact_ids::uuid[]) WITH ORDINALITY AS c(contact_id, ord)
  ON g.ord = c.ord
JOIN calendar_event ce ON ce.gcal_event_id = g.gcal_event_id
WHERE c.contact_id = ANY(ce.matched_contact_ids)
  AND ce.last_contacted_updated = true;

-- name: TestInsertContactAtID :exec
-- Latent-person promotion test support: insert a contact row AT a caller-supplied
-- id (node.id == contact.id), so a test can promote a latent person node (created
-- by EnsureLatentPerson) into a real contact at the node's id. Production
-- CreateContact generates its own id; the import/promotion pipeline that supplies
-- one is deferred per spec, so this is the test-only mechanic.
INSERT INTO contact (id, full_name) VALUES ($1, $2);

-- name: SyntheticDeleteEntityTypesByKeyPrefix :execrows
-- Graph identity cleanup: entity_type is a catalog table with no canonical_label,
-- so a test that seeds its own ns-prefixed entity_type rows clears them by key
-- prefix (the curated catalog seed rows use bare keys and are never
-- prefix-matched). Caller passes a BARE prefix; '%' is appended here.
DELETE FROM entity_type WHERE key LIKE @key_prefix || '%';

-- name: SyntheticDeletePredicatesByKeyPrefix :execrows
-- Predicate-catalog cleanup: a test that mints its own ns-prefixed provisional
-- predicates clears them by key prefix (the curated seed rows use bare keys and
-- are never prefix-matched). Caller passes a BARE prefix; '%' is appended here.
DELETE FROM predicate WHERE key LIKE @key_prefix || '%';

-- ============================================================================
-- crm-admin --reset-and-seed support: a HARD wipe of every live data table
-- so a staging instance can be reset to a known synthetic baseline. Preserves
-- only schema_migrations (the migration ledger) + River's own internal tables
-- (river_%); river_job IS wiped (stale jobs must not dereference wiped rows).
-- The list is the NET-LIVE data-table set; a non-existent relation here aborts
-- the TRUNCATE (so it must track the schema), and an omitted live data table is
-- caught by the catalog guard in the reset integration test.
-- ============================================================================

-- name: ResetSyntheticData :exec
-- HARD reset: truncate the complete live data-table closure in one statement.
-- RESTART IDENTITY resets sequences; CASCADE is a no-op safety net because the
-- list already names the full closure (it does NOT silently widen the wipe). Run
-- ONLY by crm-admin --reset-and-seed (CRM_ENV != production, service stopped,
-- --yes confirmed). The catalog guard in synthetic_reset_integration_test.go
-- fails if a public table is added that is not in this list / schema_migrations /
-- river_% / the catalog tables (predicate, entity_type).
--
-- Catalog tables (predicate, entity_type) are NOT truncated: they hold curated
-- REFERENCE data installed by migration 066, and migrations run BEFORE a reset
-- and 066 does NOT re-run on an already-migrated DB — truncating would leave an
-- empty catalog that silently breaks the assert() write path (predicate-by-key
-- → ErrNotFound) and the integration tests that call this between runs. A reset
-- MUST still restore a known baseline, so the repository's ResetSyntheticData
-- ALSO runs DeleteProvisionalPredicates + DeleteProvisionalEntityTypes (right
-- after this TRUNCATE) to clear runtime-minted PROVISIONAL rows while leaving the
-- curated catalog intact. (Namespaced test cleanup still uses
-- SyntheticDelete*ByKeyPrefix for its own provisional rows.)
TRUNCATE TABLE
    assertion,
    assertion_provenance,
    calendar_event,
    comms_message,
    contact,
    contact_enrichment,
    contact_method,
    contact_tag,
    contact_task,
    embedding,
    entity,
    event,
    event_consumer_claim,
    external_contact,
    external_identity,
    external_sync_log,
    external_sync_state,
    interaction,
    job_exec_sample,
    mac_host,
    mac_host_pairing_token,
    meeting_note,
    messages_message,
    node,
    note,
    oauth_credential,
    phone_call,
    relationship_signal,
    river_job,
    sync_staleness_breach,
    synthetic_namespace_entity,
    tag,
    telegram_channel_state,
    telegram_chat_config,
    telegram_message,
    telegram_session,
    telegram_update_state,
    venue
RESTART IDENTITY CASCADE;

-- name: DeleteProvisionalPredicates :exec
-- Part of the reset (called right after ResetSyntheticData by the repository):
-- clears runtime-minted provisional predicates so a reset restores a known
-- baseline. The curated catalog (status='curated', migration 066) is untouched.
DELETE FROM predicate WHERE status = 'provisional';

-- name: DeleteProvisionalEntityTypes :exec
-- Companion to DeleteProvisionalPredicates: clears runtime-minted provisional
-- entity subtypes; the curated subtypes (status='curated') survive.
DELETE FROM entity_type WHERE status = 'provisional';

-- name: CountNonFinalRiverJobs :one
-- Additive-seed (crm-admin --seed) preflight: count queued/in-flight river_job
-- rows (finalized_at IS NULL). An additive seed REFUSES if this is non-zero — it
-- must not steal pre-existing queued work, and a drained queue is its
-- precondition. (--reset-and-seed skips this — it WIPES river_job.) This is a
-- queue-drain precondition, NOT a live-worker liveness claim (River 0.34 does not
-- populate river_client, so no sound DB liveness signal exists).
SELECT COUNT(*) FROM river_job WHERE finalized_at IS NULL;

-- name: TestCountAllRows :one
-- Reset integration test only: count ALL rows in a single table by name. Used by
-- the clone-DB reset test to assert each wiped table is empty after the reset and
-- that schema_migrations survives. The table name is validated against the wiped
-- list by the test before it reaches here; format() with %I quotes the
-- identifier so it can never be an injection vector.
SELECT (xpath('/row/c/text()',
    query_to_xml(format('SELECT COUNT(*) AS c FROM %I', @table_name::text), false, true, '')))[1]::text::bigint;

-- name: TestListPublicTables :many
-- Reset integration test only: enumerate every base table in the public schema
-- so the catalog guard can assert each is in the wiped list, is schema_migrations,
-- or matches the river_% allowlist. Read-only catalog access.
SELECT table_name::text FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
ORDER BY table_name;

-- name: TestListIndexDefsForComms :many
-- Index-definition test only: enumerate every index on comms_message with
-- Postgres's own deterministic indexdef reconstruction, so a test can assert the
-- exact key columns + partial predicate of the eligible/stale-claim indexes
-- (migration 073). Read-only catalog access, mirroring TestListPublicTables.
SELECT indexname::text, indexdef::text FROM pg_indexes
WHERE schemaname = 'public' AND tablename = 'comms_message'
ORDER BY indexname;

-- name: TestInsertNonFinalRiverJob :exec
-- Reset/additive-seed test only: plant ONE queued (non-finalized) river_job so a
-- test can assert the additive --seed preflight REFUSES while --reset-and-seed
-- PROCEEDS (it wipes river_job). Minimal valid row: River requires kind, queue,
-- state, args, metadata; finalized_at stays NULL (the unfinalized signal).
INSERT INTO river_job (kind, queue, state, args, metadata, priority, max_attempts)
VALUES ('synthetic_test_marker', 'default', 'available', '{}'::jsonb, '{}'::jsonb, 1, 1);

-- name: TestInsertOAuthCredentialMarker :exec
-- Reset test only: a marker row in oauth_credential (the table whose preservation
-- would re-introduce real PII on re-sync). Proves the reset wipes it.
-- The token columns are bytea (encrypted-at-rest); dummy bytes are fine for a
-- marker that is never decrypted.
INSERT INTO oauth_credential
    (provider, account_id, access_token_encrypted, encryption_nonce)
VALUES ('synthetic_test', 'reset-marker', '\x00'::bytea, '\x00'::bytea);

-- name: TestInsertExternalSyncStateMarker :exec
-- Reset test only: a marker row in external_sync_state (a sync-cursor table the
-- reset wipes so staging cannot re-sync real data).
INSERT INTO external_sync_state (source, account_id)
VALUES ('synthetic_test', 'reset-marker');

-- name: TestInsertTelegramSessionMarker :exec
-- Reset test only: a marker row in telegram_session (the Telegram auth session
-- the reset wipes). session_data_encrypted + encryption_nonce are NOT NULL bytea.
INSERT INTO telegram_session (session_data_encrypted, encryption_nonce, phone_number)
VALUES ('\x00'::bytea, '\x00'::bytea, 'reset-marker');

-- name: TestInsertTagMarker :exec
-- Reset test only: a marker row in tag (a standalone table the harness does not
-- touch).
INSERT INTO tag (name) VALUES ('synthetic-reset-marker');

-- name: TestInsertDerivedStorageMarkerNode :exec
-- Reset test only: a person node with a fixed sentinel id that the embedding and
-- relationship_signal markers anchor to (relationship_signal.subject_node_id is a
-- real FK→node, so the node must exist first). Idempotent so the marker seeding
-- can re-run on a reused clone.
INSERT INTO node (id, type, canonical_label)
VALUES ('00000000-0000-0000-0000-0000000000d5'::uuid, 'person', 'synthetic-reset-marker')
ON CONFLICT (id) DO NOTHING;

-- name: TestInsertEmbeddingMarker :exec
-- Reset test only: a marker row in embedding (a derived-storage projection the
-- harness does not touch), so the reset test proves TRUNCATE empties it. The
-- vector(1536) is a zero-filled sentinel; target_id is no-FK polymorphic.
INSERT INTO embedding (target_kind, target_id, model_version, vector)
VALUES ('node', '00000000-0000-0000-0000-0000000000d5'::uuid, 'synthetic-reset-marker',
        array_fill(0::real, ARRAY[1536])::vector(1536));

-- name: TestInsertRelationshipSignalMarker :exec
-- Reset test only: a marker row in relationship_signal (a derived-storage
-- projection the harness does not touch), so the reset test proves TRUNCATE
-- empties it. subject_node_id references the marker node inserted above.
INSERT INTO relationship_signal (subject_node_id, signal_key, value, as_of, method_version)
VALUES ('00000000-0000-0000-0000-0000000000d5'::uuid, 'synthetic-reset-marker', 0, NOW(),
        'synthetic-reset-marker');

-- name: TestInsertTagForMigration :one
-- Tag-migration test only: seed a legacy tag row with an explicit name + color
-- so a test can run `--migrate-tags` over it and assert the color survives into
-- the tag entity node's detail JSONB. Returns the generated id.
INSERT INTO tag (name, color) VALUES (@name, @color) RETURNING id;

-- name: TestInsertContactTagAtTime :exec
-- Tag-migration test only: seed a contact_tag row with an explicit created_at so
-- a test can assert the migration preserves it as the assertion's KnowledgeFrom
-- (KnowledgeFromOverride). The (contact_id, tag_id) PK makes re-seeding a no-op
-- under ON CONFLICT (not needed here — tests use fresh ids).
INSERT INTO contact_tag (contact_id, tag_id, created_at)
VALUES (@contact_id, @tag_id, @created_at);

-- name: TestInsertContactTagNullCreatedAt :exec
-- Tag-migration test only: seed a contact_tag row with a NULL created_at (the
-- column is nullable), so a test can assert the migration falls back to "now" for
-- the assertion knowledge time rather than stamping a bogus zero time.
INSERT INTO contact_tag (contact_id, tag_id, created_at)
VALUES (@contact_id, @tag_id, NULL);

-- name: TestDeleteContactTagsByContactIds :execrows
-- Tag-migration cleanup: hard-delete the contact_tag rows a test seeded, keyed by
-- the tracked contact ids (scoped to the test's own contacts on the shared DB).
DELETE FROM contact_tag WHERE contact_id = ANY(@contact_ids::uuid[]);

-- name: TestDeleteTagsByIds :execrows
-- Tag-migration cleanup: hard-delete the legacy tag rows a test seeded, keyed by
-- the tracked tag ids (scoped to the test's own tags on the shared DB).
DELETE FROM tag WHERE id = ANY(@tag_ids::uuid[]);

-- name: TestCountTaggedAsAssertionsForSubject :one
-- Tag-migration test only: count the LIVE accepted `tagged_as` assertions whose
-- subject is a given node, so a test asserts exactly one per migrated contact_tag
-- and that an idempotent re-run creates no duplicates.
SELECT COUNT(*) FROM assertion
WHERE subject_node_id = $1
  AND predicate_key = 'tagged_as'
  AND status = 'accepted'
  AND knowledge_to IS NULL;

-- name: DeleteSyncStatesBySourceForTest :execrows
-- Test teardown only — hard-deletes external_sync_state rows for a given
-- source. Used by sync-service tests to scope per-source cleanup without
-- inlining raw SQL into Go test code (core.md rule 2).
DELETE FROM external_sync_state WHERE source = @source;

-- name: DeleteSyncLogsBySourceForTest :execrows
-- Test teardown only — hard-deletes external_sync_log rows for a given
-- source. external_sync_log carries its own source column (migration 011).
DELETE FROM external_sync_log WHERE source = @source;

-- name: DeleteRiverJobsBySourceArgForTest :execrows
-- Test teardown only — hard-deletes sync_provider_account river_job rows
-- whose args JSON source = @source. Mirrors the (source) JSONB path used
-- by CountInFlightSyncJobs.
DELETE FROM river_job
WHERE kind = 'sync_provider_account'
  AND (args->>'source') = sqlc.arg('source')::text;

-- name: CountRiverJobsBySourceArgForTest :one
-- Test-only count of sync_provider_account river_job rows whose args JSON
-- source = @source. Used by sync-service tests to assert enqueue/dedup
-- behavior without inlining raw SQL (core.md rule 2).
SELECT COUNT(*) FROM river_job
WHERE kind = 'sync_provider_account'
  AND (args->>'source') = sqlc.arg('source')::text;

-- name: CountActiveRiverJobsBySourceArgForTest :one
-- Test-only count of ACTIVE sync_provider_account river_job rows whose args
-- JSON source = @source. "Active" means not yet finalized (available, pending,
-- running, retryable, scheduled); completed/discarded/cancelled are excluded.
-- Used by the load test's drain-to-zero convergence check (core.md rule 2).
SELECT COUNT(*) FROM river_job
WHERE kind = 'sync_provider_account'
  AND (args->>'source') = sqlc.arg('source')::text
  AND state IN ('available', 'pending', 'running', 'retryable', 'scheduled');

-- name: CountActiveRiverJobsByStateBySourceForTest :many
-- Test-only per-state breakdown of ACTIVE sync_provider_account river_job rows
-- whose args JSON source = @source, using the SAME active-state predicate as
-- CountActiveRiverJobsBySourceArgForTest. Backs the load test's drain-timeout
-- diagnostic so a stuck backlog is reported per state (all 'available' means a
-- dead fetch loop; jobs cycling through 'running' means genuine starvation).
-- ORDER BY state for deterministic output.
SELECT state, COUNT(*) AS count FROM river_job
WHERE kind = 'sync_provider_account'
  AND (args->>'source') = sqlc.arg('source')::text
  AND state IN ('available', 'pending', 'running', 'retryable', 'scheduled')
GROUP BY state
ORDER BY state;

-- name: CountSyncStatesByStatusForTest :one
-- Test-only count of external_sync_state rows for a given source in a given
-- status. Backs the load test's quiescence assertion that no state row ends
-- in 'syncing' (core.md rule 2).
SELECT COUNT(*) FROM external_sync_state
WHERE source = @source AND status = @status;

-- name: CountSyncLogsByStatusForTest :one
-- Test-only count of external_sync_log rows for a given source in a given
-- status. Backs the load test's quiescence assertion that no log row ends in
-- 'running' (core.md rule 2).
SELECT COUNT(*) FROM external_sync_log
WHERE source = @source AND status = @status;

-- name: InsertRiverJobForTest :exec
-- Test-only seed of an in-flight sync_provider_account river_job row so
-- the atomic-claim dedup path observes count>0. Mirrors the row a real
-- enqueue would insert; the worker never runs in these tests.
INSERT INTO river_job (
    args, kind, max_attempts, priority, queue, state,
    attempt, created_at, scheduled_at
) VALUES (
    @args, 'sync_provider_account', 3, 1, 'default', 'running',
    1, NOW(), NOW()
);

-- name: TestInsertRiverJobWithStateForTest :exec
-- /health river-component integration test only: plant one river_job with an
-- explicit kind, state, scheduled_at, and (nullable) finalized_at so the
-- discarded-count / oldest-due / latest-completed-by-kind queries can be
-- asserted against known values. All timestamps are caller-supplied (no NOW())
-- so the freshness/age discrimination assertions are deterministic — explicit
-- finalized_at values hours apart are what make the latest-completed-by-kind
-- "newest wins" assertion real. Minimal valid row modeled on
-- TestInsertNonFinalRiverJob: River requires kind, queue, state, args,
-- metadata, max_attempts.
INSERT INTO river_job (kind, queue, state, args, metadata, priority, max_attempts, scheduled_at, finalized_at)
VALUES (@kind, 'default', @state::river_job_state, '{}'::jsonb, '{}'::jsonb, 1, 1, @scheduled_at, @finalized_at);

-- name: TestInsertRiverJobFullTimingForTest :exec
-- Tier-0 integration test only: plant one FINISHED river_job with explicit
-- kind, state, scheduled_at, attempted_at, and finalized_at so the
-- Tier0RiverJobStatsByKind wait (attempted_at - scheduled_at) and run
-- (finalized_at - attempted_at) percentiles can be asserted against known
-- values. All timestamps are caller-supplied (no NOW()). Extends
-- TestInsertRiverJobWithStateForTest with an explicit attempted_at (which that
-- query leaves NULL) since Tier-0's wait/run arithmetic needs it populated.
INSERT INTO river_job (kind, queue, state, args, metadata, priority, max_attempts, scheduled_at, attempted_at, finalized_at)
VALUES (@kind, 'default', @state::river_job_state, '{}'::jsonb, '{}'::jsonb, 1, 1, @scheduled_at, @attempted_at, @finalized_at);

-- name: SyntheticSelectContactIdsByFullNamePrefix :many
-- Cleanup id-set (step 1): every contact whose full_name carries the namespace
-- prefix, INCLUDING soft-deleted rows (see the soft-delete exception above).
-- Caller passes a BARE prefix; '%' is appended here. The namespace charset
-- excludes the LIKE metacharacters, so the prefix can never over-match.
SELECT id FROM contact
WHERE full_name LIKE @name_prefix || '%'
ORDER BY id;

-- name: SyntheticSelectVenueNodeIdsForContacts :many
-- Cleanup id-set (step 2): the venue nodes the real recorders minted for this
-- namespace's interactions. Venue nodes are not contacts and carry an empty
-- canonical_label, so neither the person-node delete nor the label-prefix sweep
-- finds them — they must be captured BEFORE the interaction delete removes the
-- only link. Includes soft-deleted interactions for the same reason as above.
SELECT DISTINCT venue_id FROM interaction
WHERE contact_id = ANY(@contact_ids::uuid[])
  AND venue_id IS NOT NULL;

-- name: SyntheticDeleteInteractionsByContactIds :execrows
-- Cleanup ladder: interactions by contact (hard delete — the ledger's by-id
-- variant is unavailable cross-request). Soft-deleted rows included.
DELETE FROM interaction WHERE contact_id = ANY(@contact_ids::uuid[]);

-- name: SyntheticSelectMacHostIdByHostname :one
-- Cleanup ladder + namespace occupancy: the namespace's synthetic host, looked
-- up by EXACT hostname. Deliberately not a prefix match: 'synth-foo-host' used
-- as a LIKE prefix would also match namespace 'foo-hostile''s
-- 'synth-foo-hostile-host' and delete another world's marker.
SELECT id FROM mac_host WHERE hostname = @hostname;

-- name: SyntheticSelectLiveDescendantHostnames :many
-- Cleanup descendant guard: hostnames of any namespace nested UNDER this one
-- ('synth-<ns>-%-host'). A namespace whose prefix sweep would cross into a live
-- descendant refuses to clean rather than deleting across it. Note
-- 'synth-<ns>-host' itself does not match this pattern; the namespace's own
-- salt variants do and are filtered out by the caller (they are expansion
-- members, not descendants).
SELECT hostname FROM mac_host
WHERE hostname LIKE @namespace_prefix || '%-host'
ORDER BY hostname;

-- name: SyntheticCountPendingJobsForNamespaceCleanup :one
-- Cleanup safety gate, at Gate-B parity: unfinalized River jobs that still
-- dereference this namespace's rows. Two linkage classes, both required —
-- event-linked jobs (args->>'event_id'), and messaging_aggregate_for_contact
-- jobs, which key on {contact_id, source} and carry NO event id, so an
-- event-only predicate would happily delete rows an aggregate job still
-- references. The aggregate arm is deliberately source-agnostic: a superset of
-- the two sources teardown checks, erring toward REFUSING to delete.
SELECT COUNT(*) FROM river_job
WHERE finalized_at IS NULL
  AND (
        (args->>'event_id') = ANY(@event_ids::text[])
     OR (kind = 'messaging_aggregate_for_contact'
         AND (args->>'contact_id') = ANY(@contact_ids::text[]))
  );

-- name: SyntheticTryAdvisoryLock :one
-- Namespace reservation (seed + cleanup): NON-BLOCKING session advisory lock.
-- A held lock means a concurrent run owns the namespace, which is answered
-- immediately (409 on seed, "busy" on cleanup) rather than queued behind. MUST
-- be executed on a dedicated pooled connection — session advisory locks belong
-- to the connection that took them.
SELECT pg_try_advisory_lock(@lock_key::bigint);

-- name: SyntheticAdvisoryLock :exec
-- Numeric-band claim: BLOCKING session advisory lock, bounded by the caller's
-- context deadline. Unlike the namespace reservation, contention here is
-- legitimate (two DIFFERENT namespaces hashing into the same phone area or
-- telegram peer band), and serializing them is what makes the toolkit's
-- detect-and-re-salt sound under concurrency. Same dedicated-connection rule.
SELECT pg_advisory_lock(@lock_key::bigint);

-- name: SyntheticAdvisoryUnlock :one
-- Releases a session advisory lock on the SAME connection that took it.
-- Returns false when the caller did not hold it.
SELECT pg_advisory_unlock(@lock_key::bigint);

-- name: TestInsertUnfinalizedRecorderJobForEvent :one
-- Failure-injection fixture (declared-seeding tests): plants ONE unfinalized
-- event-linked river_job so the Gate-B-parity pending check has something real
-- to refuse on. The generic TestInsertNonFinalRiverJob marker is deliberately
-- UNLINKED and Gate B does not count it, so it cannot exercise this path.
--
-- Planted as SCHEDULED an hour out rather than available: a live River client
-- would otherwise fetch and finalize it within milliseconds, and the assertion
-- that depends on it staying unfinalized would pass or fail on a race.
INSERT INTO river_job (kind, queue, state, args, metadata, priority, max_attempts, scheduled_at)
VALUES ('interaction_recorder', 'default', 'scheduled',
        jsonb_build_object('event_id', @event_id::text), '{}'::jsonb, 1, 1,
        NOW() + INTERVAL '1 hour')
RETURNING id;

-- name: TestInsertUnfinalizedAggregateJobForContact :one
-- Failure-injection fixture: the SECOND Gate-B linkage class — an aggregate job
-- keyed on {contact_id, source} with NO event id. A cleanup predicate that only
-- looked at event ids would miss it and delete rows it still dereferences.
-- Scheduled an hour out for the same reason as the recorder fixture above.
INSERT INTO river_job (kind, queue, state, args, metadata, priority, max_attempts, scheduled_at)
VALUES ('messaging_aggregate_for_contact', 'default', 'scheduled',
        jsonb_build_object('contact_id', @contact_id::text, 'source', @source::text),
        '{}'::jsonb, 1, 1, NOW() + INTERVAL '1 hour')
RETURNING id;

-- name: TestFinalizeRiverJobByID :exec
-- Failure-injection fixture: finalizes a planted job so the "refuses while
-- pending, cleans once safe" tests have an explicit, honest safe-state step
-- rather than a hand-wave.
UPDATE river_job
SET state = 'completed'::river_job_state, finalized_at = NOW()
WHERE id = @id;

-- name: TestInsertTelegramChatConfigInBand :exec
-- Failure-injection fixture (declared-seeding tests): occupies a namespace's
-- telegram peer band with a row that belongs to NO contact, which is the
-- cheapest way to force the toolkit's band-collision re-salt without seeding a
-- whole world that would then need cleaning. Reclaimed by the existing
-- chat-config delete.
INSERT INTO telegram_chat_config (telegram_chat_id, chat_type, status)
VALUES (@telegram_chat_id, 'group', 'auto');

-- name: SyntheticRecordNamespaceEntity :exec
-- Namespace ownership, written at seed time. The declared-seed endpoint seeds
-- in one request and cleans up in another, and every other id set is recovered
-- from a generator-derived token carried by the row — which for contacts means
-- full_name, a USER-EDITABLE column. A test that renames a seeded contact
-- through the ordinary contact API makes it invisible to the name-derived
-- sweeps (node.canonical_label is rewritten with it), so cleanup would skip it
-- and report success over live residue. This record is keyed by the entity's
-- own id, which nothing in the application rewrites.
INSERT INTO synthetic_namespace_entity (namespace, entity_kind, entity_id)
VALUES (@namespace, @entity_kind, @entity_id::uuid)
ON CONFLICT (namespace, entity_kind, entity_id) DO NOTHING;

-- name: SyntheticSelectNamespaceEntityIds :many
-- Cleanup id-set: the entity ids this namespace recorded ownership of. Unioned
-- with the prefix sweep rather than replacing it — the prefix still finds rows
-- a pre-ownership run seeded, and ownership still finds rows the prefix lost.
SELECT entity_id FROM synthetic_namespace_entity
WHERE namespace = @namespace AND entity_kind = @entity_kind
ORDER BY entity_id;

-- name: SyntheticDeleteNamespaceEntities :execrows
-- Cleanup ladder (final block): drops the namespace's ownership records. Runs
-- with the host-marker delete — only after every earlier step succeeded — so a
-- partially failed sweep keeps the namespace both discoverable AND recoverable.
DELETE FROM synthetic_namespace_entity WHERE namespace = @namespace;

-- name: SyntheticDeleteUnfinalizedJobsInQueue :execrows
-- Cleanup ladder: drops the namespace's own ORPHANED River jobs.
--
-- A harness client fetches only its own private queue, so a job left in that
-- queue after the client stopped can never be worked by anyone — and the
-- pending gate would refuse to sweep the namespace forever. Cleanup holds the
-- namespace reservation while this runs, so no live run owns these rows.
-- Deliberately scoped to ONE queue and to unfinalized rows: a `default`-queue
-- job belongs to the live application and must still gate the sweep, and
-- finalized rows are River's own retention business.
DELETE FROM river_job
WHERE queue = @queue
  AND finalized_at IS NULL
  AND (
        (args->>'event_id') = ANY(@event_ids::text[])
     OR (args->>'contact_id') = ANY(@contact_ids::text[])
  );

-- name: SyntheticDeleteRiverQueue :execrows
-- Cleanup ladder: drops the river_queue row a harness producer created for the
-- namespace's private queue. Without it every declared seed would leave one
-- permanent row behind on the shared database.
DELETE FROM river_queue WHERE name = @name;

-- name: SyntheticCountRiverQueue :one
-- Cleanup assertion — does a namespace's private river_queue row still exist?
-- Every harness starts a producer that upserts one, and a database reset
-- preserves River's own tables, so an un-deleted row is permanent residue.
SELECT COUNT(*) FROM river_queue WHERE name = @name;

-- name: TestInsertAvailableRematchJobForContact :one
-- Isolation fixture: plants an AVAILABLE (immediately fetchable)
-- rematch_dispatcher job on the `default` queue — the queue the live
-- application works. A harness that fetched it would be stealing another
-- process's job. Available (not scheduled) precisely because the fetch has to
-- be POSSIBLE for "it was never fetched" to mean anything.
INSERT INTO river_job (kind, queue, state, args, metadata, priority, max_attempts, scheduled_at)
VALUES ('rematch_dispatcher', 'default', 'available',
        jsonb_build_object(
          'event_id', @event_id::text,
          'contact_id', @contact_id::text,
          'rematch_job_id', @rematch_job_id::text),
        '{}'::jsonb, 1, 5, NOW())
RETURNING id;

-- name: TestGetRiverJobDispositionByID :one
-- Test assertion — a planted job's disposition: its state, whether it is
-- finalized, and its attempt counter. `attempt` is the load-bearing one for
-- queue isolation: River increments it on FETCH, so attempt = 0 says the job was
-- never handed to a worker at all — a positive statement, unlike "not
-- finalized", which a job nobody ever looked at also satisfies.
SELECT state::text AS state,
       (finalized_at IS NOT NULL)::bool AS finalized,
       attempt::int AS attempt
FROM river_job WHERE id = @id;
