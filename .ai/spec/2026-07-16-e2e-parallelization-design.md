# E2E Test Parallelization — Design (issue #425)

**Status:** proposed · **Issue:** #425 · **Predecessors:** #413 (Go suite parallelization), #380/E2E settlement arc (#667–#678, `.ai/spec/2026-07-15-remaining-e2e-migration-design.md`)

## Problem

The Playwright suite runs every worker against one shared Postgres DB, one backend, one frontend. Seeding is per-worker prefixed (`getTestPrefix` → `w{workerIndex}-{timestamp}`), but reads and assertions hit global views — the imports candidate list, the resolver modal, the dashboard overdue widget, the contacts list — where other workers' rows leak in. The suite compensates with a family of workarounds: `navigateModalToCandidate` (a linear scan across foreign candidates, 24 call sites), `.first()` disambiguation on global lists, `limit=10000` fetches to find own rows, `.or()` two-arm assertions, and invariant-only (never exact) counting on the dashboard.

That flake family is why parallelism is pinned down in exactly the lanes the user feels most:

- `make test-e2e-diff` (the pre-push lane) pins `PLAYWRIGHT_WORKERS=1` — added in #298 explicitly because of "contention on the shared imports modal pagination, the contacts list cache, and seed/cleanup races", at a measured cost of 1.3m serial vs ~30s parallel.
- `playwright.config.ts` forces 1 worker on linux/arm64 (originally for the Pi; it also serializes the dev sandbox, which is arm64 but not resource-starved).
- CI runs 4 workers × 2 shards (~2:50/shard) and is fine — CI wall-clock is not the target.

Root cause of the worst offender: `openCandidateModal` on the imports page passes `initialIndex` — an index into a live React Query list. If the list refetches between the click and the modal render (another worker's action, a window-focus refetch), the index points at a different candidate. **This is a real-user bug, not just a test problem**: a list reorder between click and open shows the wrong candidate to a human too.

## Goals

1. **Eliminate the cross-worker pollution flake class** — no test observes or is broken by another worker's data; retire the workaround family and the three related core.md gotchas.
2. **Raise workers in the local lanes**: unpin the diff lane, replace the arm64 1-worker heuristic, so pre-push and sandbox full-suite runs are parallel.
3. **Codify the conventions** so new tests are parallel-safe by construction (pattern doc + core.md updates).

## Non-goals

- Per-worker databases/backends (heavy, rejected — CI is already ~3min; namespace isolation on the shared DB suffices).
- CI wall-clock work (shards/worker counts stay 2×4).
- Any change to the Go integration suite (already parallel via #413's namespace + ephemeral-clone toolkit).
- New user-facing features (e.g. a search box on the imports page). The one product change here is a bug fix (modal keyed by id, below).

## Design

The audit (2026-07-16, this session) classified every pollution site into four buckets. Each gets a mechanism:

### 1. Resolver modal → open by candidate id (product bug fix)

Change the imports page/`suggestion-modal.tsx` contract from `initialIndex: number` to `initialCandidateId: string`: the modal derives its index from the id in the current candidates array, tracks the id (not the index) across list refetches, and keeps the existing clamp behavior when the id disappears (resolved/removed → advance in place, close when empty). `openCandidateModal(index)` call sites in `imports/page.tsx` pass the clicked candidate's id instead.

Effects on tests: `navigateModalToCandidate` (helpers/imports-helpers.ts:43–98) and its 24 call sites collapse to "click own card (prefixed name) → modal deterministically shows own candidate". `findCandidateByName`'s pagination scan stays (finding your card across a paginated global list is legitimate and already parallel-safe). The modal's "X of Y" counter stays global — tests keep never asserting absolute Y. The `.or()` close-vs-advance two-arm (imports-modal.spec.ts:782) stays for the few queue-exhaustion asserts, since the queue is inherently global; the assertions are already written to tolerate both arms.

Spec-SSOT obligations (same PR, scanner-enforced): the affected `spec/imports-matching.yaml` behaviors covering modal open/navigation get extended in place; citing indexes updated; `make spec-lint && make spec-coverage` green. This is a UI product change — the PR is flagged for human review per the "never merge UI PRs autonomously" rule, with the rest of the arc stacked on it.

### 2. Global singletons → explicit cross-file mutual exclusion

`mac_host` is a singleton table (one non-revoked host by unique index). `settings-mac.spec.ts` and `imports-interactions.spec.ts` both `resetMacHosts()` (a global DELETE) and are serial within their files — but nothing stops the two files landing in different workers and nuking each other's host mid-test. This has been surviving on luck and retries.

Mechanism: a worker-agnostic mutex fixture (`helpers/global-lock.ts`) — a lockfile acquired in `beforeEach`/released in `afterEach` (atomic `mkdir` spin loop with stale-lock timeout; boring, no deps). Both mac_host-touching describes take the same named lock (`mac-host`). Tests inside stay serial-within-file as today; the lock only adds cross-file exclusion. The attention-badge count assertions in imports-interactions are deterministic under the lock (only meeting-note seeder in the suite).

Alternatives rejected: merging the two files (couples unrelated domains, breaks test-map area mapping); a separate serial Playwright project (workers is a global setting, not per-project — would require a second playwright invocation in every lane).

### 3. Scoped-read conventions → codify what already works

The contacts list is the model citizen: scope via `?search=${prefix}` (URL param or search box), then assert relative order/membership among own rows only. Dashboard widgets: assert the internal invariant (`header count == rendered cards && >= seeded minimum`), never absolute counts or decrements. Empty states: always `page.route` mocks, never real-DB emptiness. Provider/settings surfaces: route-mock the provider boundary (already universal in settings*/telegram specs).

Work: sweep the remaining unscoped reads found by the audit (`contacts.spec.ts:186` unscoped first/last row; the `limit=10000` global scan in `imports-actions.spec.ts:242` — its `GET /imports/{id}` assert two lines earlier already proves the point, drop the scan; prefixed-`.first()` on imports lists → card-scoped locators via the existing `candidateCardByName` tightened to exact-heading match). Drop the stale serial pin on the contacts UI-create block (names are already prefixed; assertions are on the created contact's own page — verify under parallel soak before landing). Delete the `cleanupAll()` wildcard footgun (`prefix:'w'` matches every worker's data; zero call sites).

Codify in `.ai/patterns/e2e-parallelism.md` (new, ~40 lines): the prefix contract, the four scoping rules above, the global-lock fixture, and what to do when a new surface can't be scoped (mock it or lock it). core.md: retire the two "(Not yet retired: E5 …)" gotchas + the `navigateModalToCandidate` gotcha; add one pointing at the pattern doc.

### 4. Unpin the lanes + validate

- `Makefile` `test-e2e-diff`: drop `PLAYWRIGHT_WORKERS=1`.
- `playwright.config.ts`: replace the `linux+arm64 → 1` heuristic with the cpu-count formula used everywhere else. The Pi (4 cores, low RAM) would get 3 workers by formula; if Pi runs prove contended, the operator override is `PLAYWRIGHT_WORKERS=1` in the Pi's environment — the heuristic no longer punishes every arm64 box for the Pi's constraints. (Deploy flows don't run Playwright on the Pi; this is a safety note, not a blocker.)
- Latent channel to confirm inert: the E2E backend runs `ENABLE_EXTERNAL_SYNC=true` + `EVENT_BUS_INGEST_ENABLED=true`, so River/ingest workers operate on the shared DB across namespaces. No test drains the queue directly; seeded `test`-source rows shouldn't be picked up by real-provider sync. The soak validates this empirically.
- **Soak gate before the unpin lands:** full suite `PLAYWRIGHT_WORKERS=4 make test-e2e` × 5 consecutive green on the sandbox, plus a representative `make test-e2e-diff` (post-unpin) × 5. Any flake is a bug in this arc, not a rerun candidate.

## PR plan

Three stacked PRs, in dependency order:

1. **fix(imports): key resolver modal by candidate id** + retire `navigateModalToCandidate` (24 call sites) + spec/imports-matching.yaml extend-in-place + citations. *UI change → human review before merge; PRs 2–3 stack on its branch.*
2. **test(e2e): cross-file global lock + scoped-read sweep** — global-lock fixture for mac_host files, unscoped-read fixes, drop stale contacts serial pin, delete `cleanupAll`, pattern doc + core.md gotcha updates.
3. **feat(e2e): unpin diff lane + arm64 worker heuristic** — Makefile + playwright.config.ts + soak results in the PR description.

Each PR gets the high-effort code-review workflow before merge (settlement-arc pattern). Estimated size: PR1 medium (one component + one page + 6 spec files), PR2 medium (helpers + ~8 spec files + docs), PR3 small.

## Risks

- **Modal id-keying changes UX behavior subtly** (index-follow vs id-follow when the list shifts under an open modal). Mitigation: keep the existing clamp/advance semantics on id-disappearance; the change is only *which* candidate the modal tracks while it exists. Human review on PR1.
- **Unpinned diff lane re-flakes** if the sweep missed a pollution site. Mitigation: the soak gate; the diff lane can be re-pinned in one line without reverting the fixes.
- **Pi regression** from the heuristic change — accepted; `PLAYWRIGHT_WORKERS` override documented, and no automated Pi lane runs Playwright.
- **Spec-coverage churn**: moving/renaming tests keeps citations valid (scanner scans all spec files); PR1's then-list edits must update citing indexes in the same PR (enforced).
