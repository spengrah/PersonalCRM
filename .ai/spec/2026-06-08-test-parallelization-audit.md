# `backend/tests` `t.Parallel()` Audit — Classification Table

Companion to `.ai/spec/2026-06-07-test-parallelization-design.md` (Component 1 — within-run parallelism). This is the per-file classification of every `backend/tests` top-level file into one of five buckets, with the concrete action taken in the flip PR (#413). It is the done-definition for the flip: every file is classified + actioned exactly as recorded here.

## Authoritative counts

All **80** top-level `backend/tests` files are classified — zero duplicates, zero gaps.

| Bucket | Distinct files | Action |
|--------|---------------|--------|
| scoped-safe | 27 | flip the top-level funcs (some need a `gin.SetMode` hoist first — see D3.5) |
| needs-scoping | 14 | scope assertions/fixtures, then flip |
| river-heavy | 30 | stay serial (live `river.NewClient(TestOnly:true)` shares the package clone's `river_job`) |
| inherently-serial | 4 | stay serial (process globals / fixed-id singletons / shared on-disk fixtures) |
| mixed | 5 | flip per-func (scoped-safe + scoped funcs) and leave the river/serial funcs serial |
| **Total** | **80** | |

**Reclassifications found during the `-count=10`/`-shuffle` validation (the audit's static read missed these):**
- `telegram_discovery_upsert_test.go` — audit said scoped-safe, but its funcs share fixed source_ids (`tg-discovery-upsert-<name>`) and a prefix-wide setup cleanup, plus a fixed peer `99001`/chat `9900` in the batch func. Moved to **needs-scoping**: per-test-unique source_id prefix (`syntheticNS`) + `uniqueTestIDs`-derived peer/chat. So scoped-safe drops to 27, needs-scoping rises to 14.
- `gmail_enablement_reconcile_test.go`'s `TestResetGmailBackfillCursors_ResetsOnlyEnabledEmailStates` — `ResetGmailBackfillCursors` scans/mutates ALL enabled email states DB-wide and asserts an exact Scanned/Reset count. **That one func stays SERIAL** (inherently global); the file's other 9 funcs flip.
- Two `ListContacts(limit 100)` membership checks (`cadence_filter`, `interaction_direction` `TestFollowupFilter`) overflowed the page once the shared DB accumulated >100 contacts under `-count=10`; raised the limit so own-ID membership stays in-window.
- `contact_task` `TestContactTask_CountByProvider` used a DB-wide `DeleteContactTasksByProvider("todoist")` cleanup that deleted parallel tests' tasks; scoped to delete only its own task IDs.

Notes on the count:
- 4 of the 28 scoped-safe and 1 of the 5 mixed entries are helper/zero-func files (`gmail_time_helpers_test.go`, `synthetic_migration_helpers_test.go`, `testmain_integration_test.go`, `test_event_bus_harness_test.go`) — no `TestXxx` to flip; listed for completeness.
- **River-heavy = 30 distinct files.** An earlier working pass double-listed `gmail_golive_integration_test.go` (identical river-heavy/serial classification both times); it is one file.
- **7 files are `RequireLongTests`-gated** (the `synthetic_*` provider/replay/profile/seed/golden set) and skip the fast suite entirely. Leaving them serial has zero effect on fast-suite / pre-push wall-clock.
- **Scoping work before flip = 15 actions:** the 13 pure needs-scoping files + the needs-scoping sub-work in the 2 mixed files `identity_integration_test.go` and `integration_test.go`.

## Key planning findings

- **The parallelization win comes entirely from the non-long-gated scoped-safe + needs-scoping files.** The 7 long-gated files skip the fast suite; the 30 river-heavy + 4 inherently-serial files stay serial.
- **~23 non-long-gated river-heavy files still run in the fast suite and stay serial.** These (`address_book_reconcile`, `calendar_decline_cutover`, `comms_gchat_engine`, `consumer_interaction_recorder`, `email_interaction`, `followup_apply_interaction`, `followup_create_worker`, `followup_manager_cutover`, `gchat_golive`, `gmail_correspondence`, `gmail_golive`, `gmail_provider`, `gmail_rematch`, `rematch_dispatcher_unique_opts`, `rematch_event_dedup`, `rematch_integration`, `river_integration`, `suggestion_service`, `sync_repo_enqueue`, `synthetic_reset`, the three `telegram_aggregation*` files, plus the river-heavy funcs of `event_bus`/`gchat_provider`/`synthetic_golden`) carry ~90+ test funcs and dominate fast-suite serial time. They cannot be sped up without per-test River-client isolation — out of scope for this flip (the recorded future lever; tracked as #428).
- **Go scheduling semantics for mixed packages:** within one package, all non-`t.Parallel()` funcs run to completion (sequentially) FIRST, and only then does the `t.Parallel()` cohort run concurrently. So the serial River funcs gate the parallel cohort rather than overlapping it. This is the Amdahl floor — the serial fraction is paid in full before any parallel speedup.
- **Helper/zero-func files:** `gmail_time_helpers_test.go`, `synthetic_migration_helpers_test.go`, `testmain_integration_test.go` (TestMain only), `test_event_bus_harness_test.go` (the shared River harness every river-heavy file depends on) have zero `TestXxx` — nothing to flip.
- **Inherently-serial conversion opportunities (out of scope here):** `telegram_state_storage` (fixed consts 99999/88888) and `unit_test.go`'s `gin.SetMode` global could in principle be reworked, but `telegram_session` (id=1 singleton) is genuinely unconvertible. Left serial.

## Subpackage confirmation / deferral

This audit covers ONLY the top-level `backend/tests` package (one Go package, one `TestMain` clone). The fast suite (`make test-integration-fast`) also compiles and runs these sibling packages — their handling:

- **CONFIRMED serial — `tests/river` (2 funcs), `tests/scheduler` (1 func):** river-heavy by construction (live River clients). Stay serial; no parallelism benefit. This matches the design spec's "already safe" outcome for the no-benefit case.
- **DEFERRED — `tests/api` (~241 funcs) and `tests/unit` (64 funcs):** NOT covered by this audit. `tests/api` is a large, genuinely parallelizable opportunity, but flipping it correctly needs the SAME per-file audit (handler tests, shared `gin` mode, DB-backed vs `httptest`, fixed-id fixtures). That audit does not exist yet. Tracked as a follow-up (#429). This PR does not assert these are "safe or trivially convertible."
- **DEFERRED — `internal/google`, `internal/todoist`:** mock-based, mostly no DB, low parallelism yield. NOT audited; NOT claimed safe.

## Needs-scoping actions (do these before flipping the file)

Each action matches the existing toolkit/namespace pattern already used in the file (`syntheticNS(t)` / `migrationGenerator(t).Prefix()` / `gen.Prefix()`); a local per-test `uuid` suffix where the file does not import the toolkit. No raw SQL — a fixture that genuinely needs a new query gets a test-only sqlc query + repository wrapper.

1. **`cadence_filter_integration_test.go`** — Scope the `CountContacts_HasCadence` sub-test's `totalHas`/`totalNo` (currently unscoped DB-wide `CountContacts("has_cadence","")>0`) to count only this test's own `syntheticNS`-prefixed rows (or restate as a before/after delta). List/ID sub-tests already ID-scoped.
2. **`contact_by_integration_test.go`** — Suffix all FIXED `full_name` fixtures (`Test Weekly Contact`, `Overdue Contact`, `Very Overdue Contact`) with a per-test suffix. File does NOT import the toolkit → add a local `uuid`-suffix mechanism. Per-id membership/index asserts already safe.
3. **`contact_task_integration_test.go`** — Suffix the FIXED `ExternalTaskID` literals (`12345`,`11111`,`33333`,`44444`,`55555`,`action-task-1`, …) with the per-test ns so `GetContactTaskByExternalID` + the `(provider, external_task_id)` unique/upsert keys don't collide. FK contacts already scoped; `CountByProvider` `GreaterOrEqual` is invariant-safe.
4. **`contact_task_service_test.go`** — Replace fixed Todoist sync-state `AccountID "test-account-123"` + fixed contact name `Test Contact for Service` with per-test unique values, and update the `DeleteSyncStatesByAccountID` pre-clean to target the unique AccountID. `SetTodoistClientFactory` is per-instance → does NOT force serial.
5. **`external_contact_soft_delete_sweep_test.go`** — Rescope the two `CountHiddenUnresolvedTelegram(...) == 1` asserts in `TestExternalContactUnmatched_HidesUnresolvedTelegramByDefault` (DB-wide `COUNT(*) WHERE source='telegram'`, no prefix filter) to count only this test's prefixed rows, or assert a delta. All other funcs already `syntheticNS`-scoped.
6. **`gchat_enablement_reconcile_test.go`** — `TestReconcileGChat_PerAccountShape_NoNullAccountRow` asserts `GetSyncStateBySource('gchat', nil) == ErrNotFound` (DB-wide "no `(gchat, NULL)` row anywhere"). **Decision: keep that one func serial** as an invariant guard; flip the other 9 `uniqueAccount`-scoped funcs.
7. **`interaction_direction_test.go`** — Give `TestFollowupFilter`'s two contacts namespaced `full_name`s (it scans the whole table via `has_followup`/`no_followup` with fixed names → fuzzy-collision risk); suffix `TestCompletedCadenceTask_CanBeReplacedByNewOne`'s fixed `ExternalTaskID` literals (`original-cadence-task`/`replacement-cadence-task`) with the per-test ns. Other 6 funcs already `contact.ID`-scoped.
8. **`interaction_source_descriptor_check_test.go`** — Narrow `TestInteractionSourceCheck_AcceptsPhoneCalls`'s deferred `HardDeleteInteractionsBySourceRefPrefix` from fixed `phone-calls-test-%` to `phone-calls-test-<syntheticNS>%`. The descriptor-agreement func is read-only.
9. **`interaction_source_gchat_check_test.go`** — Narrow the deferred `HardDeleteInteractionsBySourceRefPrefix(GChat, …)` from fixed `gchat-test-%` to `gchat-test-<syntheticNS>%`.
10. **`interaction_source_messages_check_test.go`** — Narrow `TestInteraction_SourceCheckAcceptsMessages`'s deferred cleanup from fixed `messages-test-%` to `messages-test-<syntheticNS>%`. `RejectsWhatsapp` already scoped.
11. **`search_integration_test.go`** — Give each created contact/note a unique suffix (`syntheticNS(t)`) so the FTS query term is unique, replacing FIXED `full_name`s (`Alice Johnson`,`Bob Smith`,`Michael Johnson`,`Sarah Michael`) and search terms (`Michael`,`golang`,`Pagination`). Convert global-shape asserts to asserts over only the test's own rows queried with its unique term.
12. **`telegram_chat_config_test.go`** — Replace package-level fixed `testChatID1/2/3` (70001-70003) with per-test unique chat IDs; make each func's cleanup target only its own IDs; convert global-list asserts to scoped lookups.
13. **`telegram_message_test.go`** — Replace fixed message IDs (90001-90005) and fixed chat IDs (12345, -100555, 77777) with per-test unique ids; rework `cleanupTelegramMessages` to delete only the test's own ids/chat range; change `TestTelegramMessage_ListUnprocessed` to assert on its own scoped chat.

Mixed-file scoping (the needs-scoping sub-work in 2 mixed files):

14. **`identity_integration_test.go`** — `TestIdentityRepository_Integration`: replace fixed identifiers + fixed source `test_source` with per-test uuid-suffixed values; the upsert accumulates `message_count = old + EXCLUDED`, so the exact `message_count == 6` assert and the `ListUnmatched idx1<idx2` ordering assert must be scoped. `TestIdentityService_Integration`: replace fixed emails/phone and fixed `test_discovery`/`test_cache` sources. `NormalizationPolicy` already collision-free.
15. **`integration_test.go`** — `TestContactRepository_Integration` (`ListContacts`: scope membership to own-IDs); `TestSyncRepository_Integration` (suffix fixed `Source` strings; own-IDs; `ListRecentSyncLogs len>=2` → scope to own sources); `TestOAuthRepository_Integration` (suffix provider/account; `Count == 2` → scope to suffixed provider; membership → own-IDs). `TestContactMethodRepository_Integration` + `TestFindSimilarContactsBatch_Integration` already own-scoped.

## Per-file classification

Format: `file` — **bucket** (func count; river; long-gated) — action.

### scoped-safe (27 files)

(One file originally listed here as scoped-safe — `telegram_discovery_upsert_test.go` — was reclassified to **needs-scoping** during validation; see the reclassification note near the top. Its entry below is retained for the audit trail with the corrected action.)

- `cadence_test.go` — scoped-safe (5; no; no) — flip all. Pure in-process cadence-package unit tests; no DB/River/globals.
- `calendar_decline_removal_integration_test.go` — scoped-safe (11; no; no) — flip all. `newDeclineTestEnv` runs declines synchronously in-tx (no River); per-test contact keyed by `uuid` + `gen.Prefix()` accountID; concurrent-recompute func locks only its own contact.
- `calendar_event_integration_test.go` — scoped-safe (2; no; no) — flip both. No River; namespaced seeds + per-test `event-`+uuid gcal IDs; own-row assertions. (Carries 2 inline `DatabaseConfig` literals → conn fields lowered in the tune commit.)
- `comms_gchat_identity_test.go` — scoped-safe (1; no; no) — flip. `setupCommsMessageTest` repos only; `gen.Prefix()` suffixes; membership-only assertions over own rows.
- `comms_gchat_query_test.go` — scoped-safe (2; no; no) — flip both. `migrationGenerator(t).Prefix()` on every contact/space/external_id; scoped `Len`/`Contains` membership.
- `comms_message_repository_test.go` — scoped-safe (12; no; no) — flip all. `gen.Prefix()` namespace; own-row assertions; the one DB-wide `ListEmailIdentitiesForSync` is filtered to own emails before `ElementsMatch`.
- `consumer_cadence_updater_cutover_integration_test.go` — scoped-safe (9; no; no) — flip all. `consumer.NewCadenceUpdater` directly (no River client); own-contact assertions; fresh `uuid` event ids.
- `gmail_enablement_reconcile_test.go` — scoped-safe (10; no; no) — flip 9; `newEventBusTestDB` (plain DB ctor, NO River); `uniqueAccount()` per account; per-account state reads. **Exception: `TestResetGmailBackfillCursors_ResetsOnlyEnabledEmailStates` stays SERIAL** (DB-wide scan + exact Scanned/Reset count — see the reclassification note above).
- `gmail_time_helpers_test.go` — scoped-safe (0; no; no) — no-op. Helper only.
- `ingest_registry_guard_static_test.go` — scoped-safe (2; no; no) — flip both. Static guard test; `t.TempDir()` fixtures, no DB/River.
- `interaction_integration_test.go` — scoped-safe (4; no; no) — flip all. Nil-bus ContactService (no River); contact-scoped `CountContactInteractions`. (Carries an inline `DatabaseConfig` literal → conn fields lowered in the tune commit.)
- `interaction_repository_source_filter_test.go` — scoped-safe (1; no; no) — flip. `migrationGenerator` + per-test stripe+seed source_refs; own-prefix cleanup + assertions.
- `jsonb_gin_index_test.go` — scoped-safe (5; no; no) — flip all. Per-test `uuid` prefix on every source_id/gcal_event_id; every DB-wide result narrowed by prefix before `Len`.
- `link_contact_curated_status_integration_test.go` — scoped-safe (6; no; no) — **hoist `gin.SetMode` to TestMain (D3.5), then flip all 6.** Nil-bus handler; `abSuffix(t)=syntheticNS(t)`; own-row `MatchStatus` assertions.
- `messages_message_repository_test.go` — scoped-safe (9; no; no) — flip all. `migrationGenerator` + nil-bus seed; `gen.Prefix()` guids; per-contact assertions.
- `phone_call_repository_test.go` — scoped-safe (5; no; no) — flip all. Same lightweight pattern; namespaced ids; per-contact assertions.
- `sole_writer_static_test.go` — scoped-safe (3; no; no) — flip all. Pure go/ast + file-read static guards; read-only.
- `sqlc_select_list_static_test.go` — scoped-safe (4; no; no) — flip all. Pure source-SQL lint; read-only + inline string fixtures.
- `synthetic_factory_test.go` — scoped-safe (8; no; no) — flip all. Pure factory unit tests; read-only `fixedAnchor`.
- `synthetic_migration_helpers_test.go` — scoped-safe (0; no; no) — no-op. Helpers only.
- `telegram_discovery_upsert_test.go` — **needs-scoping** (10; no; no) — RECLASSIFIED. Was assumed scoped-safe, but funcs share fixed source_ids + a prefix-wide setup cleanup, plus a fixed peer/chat in the batch func. Scoped via a per-test source_id prefix (`syntheticNS`) + `uniqueTestIDs` peer/chat, then flip all.
- `telegram_import_suggestion_test.go` — scoped-safe (1; no; no) — **hoist `gin.SetMode` to TestMain (D3.5), then flip.** `wireCadenceUpdaterForTest` (no River); `uniqueSuffix()` names; own-ext.ID assertions.
- `telegram_matcher_enrichment_test.go` — scoped-safe (7; no; no) — flip all. PeerMatcher/Identity/Enrichment only (no River); `uniqueTestIDs` namespace; even the concurrent-23505 func races within one test on its own contact.
- `telegram_message_all_fields_test.go` — scoped-safe (1; no; no) — flip. Namespaced FK contact; pre-purge + own-row read; no other test uses chat 920001.
- `telegram_message_claim_test.go` — scoped-safe (7; no; no) — flip all. `gen.PeerBandStart()` disjoint chatID + scoped `HardDeleteByChatIDRange`; own-row reads.
- `telegram_sparse_entity_enrichment_test.go` — scoped-safe (7; no; no) — flip all. `uniqueTestIDs(t,ns)` unique int64 ids; scoped cleanup; per-test handler (no package global); no River.
- `template_isolation_integration_test.go` — scoped-safe (1; no; no) — flip. `testdb.NewEphemeralClone(t)` isolated clone; sentinel asserted only by unique ID.
- `testmain_integration_test.go` — scoped-safe (0; no; no) — no test funcs (TestMain only). **Receives the `gin.SetMode` hoist (D3.5).**

### needs-scoping (14 files)

(Plus `telegram_discovery_upsert_test.go`, reclassified from scoped-safe — listed in the scoped-safe section above with its corrected action.)

(See "Needs-scoping actions" above for the per-file action. All are river=False, long-gated=False.)

- `cadence_filter_integration_test.go` (1) · `contact_by_integration_test.go` (5) · `contact_task_integration_test.go` (5) · `contact_task_service_test.go` (2) · `external_contact_soft_delete_sweep_test.go` (12) · `gchat_enablement_reconcile_test.go` (10; 1 func kept serial) · `interaction_direction_test.go` (8) · `interaction_source_descriptor_check_test.go` (2) · `interaction_source_gchat_check_test.go` (1) · `interaction_source_messages_check_test.go` (2) · `search_integration_test.go` (2) · `telegram_chat_config_test.go` (7) · `telegram_message_test.go` (7).

### mixed (5 files)

- `event_bus_integration_test.go` — mixed (10; river=True; no) — flip the 5 `TestEventRepository_*` (`newEventBusTestDB`, no River, `uniqueSourceID(uuid)`-scoped); leave the 5 `TestBus_*` serial (`newEventBusTestBus` Start()s a River client).
- `gchat_provider_integration_test.go` — mixed (18; river=True; no) — flip the 17 `setupGChatProviderTest` funcs (scoped-safe, namespaced); leave `TestGChatRematchHandler_ScansAndAggregates` serial (`setupGChatEngineTest` → live River client).
- `identity_integration_test.go` — mixed (3; river=False; no) — scope `TestIdentityRepository_Integration` + `TestIdentityService_Integration` (action 14), then flip all 3 (`NormalizationPolicy` already safe via tx-rollback + uuid suffixes).
- `integration_test.go` — mixed (6; river=False; no) — keep `TestRunMigrations_Integration` + `TestMigration020_UnifyEmailDedup` serial (schema/trigger mutation on the shared DB); scope `TestContactRepository_Integration` + `TestSyncRepository_Integration` + `TestOAuthRepository_Integration` (action 15), then flip those 3 + the already-safe `TestContactMethodRepository_Integration` + `TestFindSimilarContactsBatch_Integration`. (Carries an inline `DatabaseConfig` literal → conn fields lowered in the tune commit.)
- `synthetic_golden_scenario_integration_test.go` — mixed (2; river=True; long-gated=True) — both funcs stay serial (one River via `NewHarnessForNamespace`; one process-global `-update` flag + shared on-disk golden). No flip.

### river-heavy (30 files) — all stay serial

`address_book_reconcile_integration_test.go` (15) · `calendar_decline_cutover_integration_test.go` (2) · `comms_gchat_engine_test.go` (6) · `consumer_interaction_recorder_integration_test.go` (7) · `email_interaction_integration_test.go` (10) · `followup_apply_interaction_integration_test.go` (3) · `followup_create_worker_integration_test.go` (5) · `followup_manager_cutover_integration_test.go` (8) · `gchat_golive_integration_test.go` (1) · `gmail_correspondence_integration_test.go` (3) · `gmail_golive_integration_test.go` (1) · `gmail_provider_integration_test.go` (13) · `gmail_rematch_integration_test.go` (9) · `rematch_dispatcher_unique_opts_test.go` (1) · `rematch_event_dedup_test.go` (1) · `rematch_integration_test.go` (13) · `river_integration_test.go` (4) · `suggestion_service_integration_test.go` (13) · `sync_repo_enqueue_test.go` (6) · `telegram_aggregation_explicit_reply_bridge_test.go` (1) · `telegram_aggregation_inbound_coalesce_test.go` (1) · `telegram_aggregation_test.go` (9) · `test_event_bus_harness_test.go` (0; shared River harness, no funcs) · **long-gated:** `synthetic_gcal_provider_integration_test.go` (6) · `synthetic_migration_transform_r_reference_test.go` (1) · `synthetic_profile_integration_test.go` (5) · `synthetic_replay_integration_test.go` (5) · `synthetic_reset_integration_test.go` (2; build-tag/Short, not long) · `synthetic_seed_all_integration_test.go` (3) · `synthetic_telegram_provider_integration_test.go` (7).

Each builds a live `river.NewClient(TestOnly:true)` and/or drains `river_job` on the shared package clone; concurrent clients would steal each other's jobs. Per-test River isolation is the future lever (#428), out of scope here.

### inherently-serial (4 files) — all stay serial

- `crm_marker_construction_static_test.go` (3) — writes a shared on-disk probe into `backend/internal/todoist` and shells out to a tree-wide guard script (shared filesystem state).
- `telegram_session_test.go` (5) — `telegram_session` is a hard singleton (every query `WHERE id = 1`); no identifier to namespace.
- `telegram_state_storage_test.go` (5) — package-level fixed consts `testUserID=99999`/`testChannelID=88888` shared across funcs; convertible in principle but not in scope.
- `unit_test.go` (5) — every func calls `gin.SetMode(gin.TestMode)` (process-global write); stays serial in this PR (the `gin.SetMode` hoist that unblocks the two DB-backed handler files does not flip `unit_test.go` here).

## Baseline / after timings

_Measured on a 12-core dev box (GOMAXPROCS=12), Postgres `max_connections=100`, with a freshly-cleaned test DB. CI timing is reported separately from the CI run logs._

| Metric | Before (`main`) | After (flip, tuned) | Ratio |
|--------|-----------------|---------------------|-------|
| (a) `make test-integration-fast` wall-clock | ~29s | ~26s | ~1.12× |
| (b) `go test ./tests/ -parallel 4` package wall-clock | ~20s (17.0s internal) | ~17s (16.0s internal) | ~1.18× |

At baseline the fast-suite wall-clock (~29s) is set by `tests/api` (~24-27s internal, deferred to #429), which runs concurrently above the `tests` package this PR speeds up. So the flip's win shows up most directly in metric (b), and only partially in (a) until `tests/api` is also parallelized.

Amdahl note: the whole-package (b) speedup is bounded by the serial River floor (~23 non-long-gated river-heavy files + 4 inherently-serial + the mixed files' serial funcs). Go runs the serial cohort to completion BEFORE the parallel cohort, so the River floor is paid in full first; the modest ~1.18× whole-package ratio is the correct, expected outcome (the parallelizable subset alone is much faster). The residual is the deferred per-test-River-isolation effort (#428), not a regression. There is no fixed-multiplier gate — the honest measured number ships as-is.

**Flake validation (all on the flipped set, river-heavy serial funcs run alongside):** `-race -parallel 4` PASS (no data races); `-parallel 4 -count=10` PASS; `-shuffle=on -parallel 4` PASS; combined `-race -shuffle=on -parallel 4 -count=5` PASS. Exception: the 5 `TestIntegration_CreateWorker_*` funcs in `followup_create_worker_integration_test.go` (river-heavy, SERIAL, untouched by this PR) fail under `-count>=2` because they use fixed `external_task_id` literals (`real-123`, etc.) that collide on the global `unique_external_task_id` on the 2nd iteration. This reproduces identically in isolation and on `main` — a pre-existing `-count` limitation of the serial river suite, orthogonal to the flip; tracked with the River parallelization effort (#428). The `-count` gates above `-skip 'TestIntegration_CreateWorker'` for that reason; the normal `-count=1` fast + LONG_TESTS suites run it and pass.
