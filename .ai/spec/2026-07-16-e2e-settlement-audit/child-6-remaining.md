# E2E settlement audit — child 6 work-list (gchat-contact-signal, overdue-contact-updates, rematch-on-add-email)

Audit date: 2026-07-16. Rubric: `.ai/spec/2026-07-15-remaining-e2e-migration-design.md` ("The settled target shape", "The relaxation rubric", "Citation + coverage-check contract") + `spec/README.md`. Scanner snapshot: `make spec-coverage` 2026-07-16 (95 ui orphans corpus-wide, 0 invalid citations).

Files audited (all `frontend/tests/e2e/`): `gchat-contact-signal.spec.ts` (2 tests), `overdue-contact-updates.spec.ts` (6 tests), `rematch-on-add-email.spec.ts` (1 test). Domains actually exercised: cadence-followup (CAD), calendar (CAL), plus dashboard/contacts surfaces owned by other children's behaviors. Despite the assignment's guess, `gchat-contact-signal.spec.ts` proves no imports-matching behavior and no ingest ui behavior — it exercises the CAD-029 direction-signal surface and the interactions HTTP contract (Go-owned).

Test-map status: all three files are mapped (`test-map.json` lines ~79, ~488, ~492). If the gchat file is deleted per below, remove its test-map entry in the same PR (the pre-push `test-map-coverage-check.sh` only blocks on unmapped spec files, but a dead pattern is clutter).

Visual-guard budget used by this file set: 0.

## 1. Per-test triage table

### 1.1 gchat-contact-signal.spec.ts — `contact detail page shows direction signals after a mutual interaction` (line 36)

| Assertion (short) | Verdict | Notes |
|---|---|---|
| `getByRole('heading', { name: 'GChat Signal Test' })` visible | DELETE | Seeded-data locate — fine in style, but the whole test is a duplicate (below) |
| `getByText('Last outreach:')` visible | DELETE | Label-only duplicate of contact-direction.spec.ts:23 (`// spec: CAD-029[0], CAD-029[1]`), which asserts the stronger label+value form `/Last outreach: \S+/` |
| `getByText('Last response:')` visible | DELETE | Same — CAD-029[1] already covered with a stronger assertion |

Test-level outcome: **delete test**. It is byte-for-byte the same scenario as contact-direction.spec.ts `shows direction signal timestamps after mutual interaction` (same seed shape, same manual-source mutual POST, weaker assertions). Nothing gchat-specific is exercised — the interaction is `source=manual`; the file's own header concedes a gchat-source interaction cannot be seeded through the UI or public API. CAD-029[0]/[1] coverage is unaffected (stays cited in contact-direction.spec.ts).

### 1.2 gchat-contact-signal.spec.ts — `interactions API surfaces description + direction + source (gchat description shape)` (line 65)

| Assertion (short) | Verdict | Notes |
|---|---|---|
| POST create ok; body `direction`/`description` echo | MOVE | Covered: `backend/tests/api/interaction_test.go` `TestInteraction_CreateAndList` (create returns `source=manual` + description) and `backend/tests/api/direction_api_test.go` `TestInteractionAPI_DirectionInResponse` |
| GET list non-empty; row has `description`/`direction`/`source` properties | MOVE | Covered: `TestInteraction_CreateAndList` list subtest (rows + pagination meta) |
| `description` contains "GChat"/"messages" | MOVE | The real "GChat … (N messages)" description is produced by aggregation and asserted end-to-end in `backend/tests/comms_gchat_engine_test.go:221` (`"GChat outreach (3 messages)"`); this test only round-trips a hand-written string through the manual API, proving nothing about gchat |

Test-level outcome: **delete test**. Pure API test (no `page` at all) living in Playwright; every fact is already owned by existing Go tests — no new Go test needs authoring.

File-level outcome: **delete `gchat-contact-signal.spec.ts` entirely** and remove its `test-map.json` entry. Domain impact: none — ingest has zero `surface: ui` then-items, and CAD-029 remains covered by contact-direction.spec.ts.

### 1.3 overdue-contact-updates.spec.ts — `should remove contact from dashboard when marked as contacted` (line 71)

Uncited today.

| Assertion (short) | Verdict | Notes |
|---|---|---|
| `heading 'Action Required'` visible | DELETE | Static copy used as a settle signal; the sanctioned settle pattern is the content-predicate `waitForResponse` + sentinel used by test 1.6/dashboard.spec.ts |
| `heading contactName` visible | DELETE | Seeded-data precondition — fine in style, but the test is superseded (below) |
| card scope `locator('div.rounded-lg')` + button click | DELETE | CSS-class driver (see §2 item 1); superseded test |
| `heading contactName` not visible after click | DELETE | Would be CAD-028[1] — already cited+proven by dashboard.spec.ts:406 with a no-reload sentinel and content-predicate refetch wait |
| API `isContactOverdue === false` | DELETE | Same — CAD-028[1] backend cross-check duplicated |
| `last_contacted` within before/after wall-clock bracket; not `T00:00:00Z` | MOVE | Covered: dashboard.spec.ts CAD-028[0] block (server-assigned `occurred_at` inside click bracket, sub-second) AND Go `backend/tests/api/last_contacted_test.go` `TestPostInteraction_DirectionMutual_BumpsLastContacted` (~now on the accelerated clock) |

Test-level outcome: **delete test** — a strictly weaker duplicate of dashboard.spec.ts `marking contact as contacted updates dashboard immediately without navigation` (`// spec: DSH-005[0], CAD-028[0], CAD-028[1]`).

### 1.4 overdue-contact-updates.spec.ts — `should reflect in Contact Detail page after navigation` (line 112)

Uncited today. Misnamed: it never visits the contact detail page.

| Assertion (short) | Verdict | Notes |
|---|---|---|
| `heading 'Action Required'` visible | DELETE | Static copy settle signal |
| `waitForResponse` POST `/contacts/:id/interactions` | DELETE | Duplicate of dashboard.spec.ts's stronger version (which also asserts `direction=mutual` in request AND response) |
| `heading contactName` not visible | DELETE | CAD-028[1] duplicate |
| API `isContactOverdue === false` | DELETE | CAD-028[1] duplicate |

Test-level outcome: **delete test** — strict subset of 1.3, which is itself a subset of the dashboard.spec.ts cited test.

### 1.5 overdue-contact-updates.spec.ts — `should update last_contacted and remove from dashboard when marked from Contact Detail` (line 139)

Uncited today. This is the ONLY test in the suite driving the contact-detail-page "Log Interaction" modal to clear overdue state — a unique action surface (dashboard variant = CAD-028, list-row variant = CON-044, detail-page variant = **no SSOT behavior exists**; see §4 gap G1).

| Assertion (short) | Verdict | Notes |
|---|---|---|
| dashboard `heading 'Action Required'` visible | DELETE | Static copy; the initially-overdue precondition is already proven by the next row |
| dashboard `heading contactName` visible | KEEP | Seeded-data precondition: contact renders as overdue before the action; cite new behavior (§4 G1) then-item on the overdue-clearing outcome instead if preferred — no citation for a bare precondition |
| detail `heading contactName, level 2` visible | KEEP | Seeded-data settle on the detail page; already role-based |
| `waitForResponse` POST `/contacts/:id/interactions` (via modal) | KEEP | Network observation of the modal's write; needs rewrite: also assert `postDataJSON()?.direction` is mutual (or absent→defaults mutual) to pin what the modal sends; cite new behavior G1 |
| `getByRole('dialog')` visible; `button 'Log'` click | KEEP | Role-based driver, already compliant |
| API `isContactOverdue === false` | KEEP | Backend-state cross-check; cite new behavior G1 |

Test-level outcome: **keep + rewrite** (drop the 'Action Required' copy assertion, strengthen the POST observation), and **mint the missing behavior** (§4 G1: proposed `CON-053`) in the same PR, then cite it here. Do not cite CAD-028 — its `when` is the dashboard card action.

### 1.6 overdue-contact-updates.spec.ts — `all views should show consistent state after marking as contacted` (line 173)

Cited today: `// spec: CAD-028[2]` (line 177). This is the test dashboard.spec.ts's own comment points at for the cross-view leg.

| Assertion (short) | Verdict | Notes |
|---|---|---|
| sentinel seed + content-predicate `waitForResponse` on `/contacts/overdue` (target absent, sentinel present) | KEEP | Exemplary data-derived settle; CAD-028[2] |
| `heading sentinelName` visible; `heading contactName` not visible | KEEP | Dashboard leg of CAD-028[2]; role-based, seeded data |
| contacts list: `locator('tr').filter({hasText})` row; `td.nth(4)` toHaveText(todayShort) | KEEP (rewrite) | Data assertion in the right place, but `nth(4)` hardcodes column order (DOM-structure coupling). Rewrite: derive the column index from the table header (locate the `columnheader` whose text is the Last-response column, use its index), or scope via `getByRole('row')`/`getByRole('cell')`. Keep the row-scoped date-value assertion |
| detail: `getByText('Last response:')` visible | KEEP | Conditional data-driven row (rendered only when a response timestamp exists) — this is CAD-029[1]; add the citation (§5) |
| API `isContactOverdue === false` | KEEP | Backend cross-check leg of CAD-028[2] |

Test-level outcome: **keep + light rewrite** (header-derived column index). Note the interaction is logged via raw API rather than the UI button — acceptable: CAD-028[2]'s browser value is the three views reflecting the change, and the button-path halves ([0]/[1]) are cited in dashboard.spec.ts.

### 1.7 overdue-contact-updates.spec.ts — `last_contacted should have full timestamp precision, not just date` (line 253)

Uncited today.

| Assertion (short) | Verdict | Notes |
|---|---|---|
| `heading 'Action Required'` visible | DELETE | Static copy settle |
| card `div.rounded-lg` scope + click; heading gone | DELETE | Duplicate of dashboard.spec.ts CAD-028[1] |
| `last_contacted` in bracket; not midnight; sub-second regex `/T\d{2}:\d{2}:\d{2}\.\d+Z$/` | MOVE | Covered: dashboard.spec.ts CAD-028[0] block (server-assigned, in-bracket, sub-second, not date-only) + Go `TestPostInteraction_DirectionMutual_BumpsLastContacted` |

Test-level outcome: **delete test** — the midnight-bug regression guard lives on in dashboard.spec.ts (cited CAD-028[0]) and the Go API test; nothing here is unique.

### 1.8 overdue-contact-updates.spec.ts — `should show multiple overdue contacts on dashboard` (line 318)

Cited today: `// spec: CAD-023` (bare, line 331 — claims all 3 then-items; the test only proves [0] and [1]).

| Assertion (short) | Verdict | Notes |
|---|---|---|
| `heading First/Second Overdue` visible | KEEP | Seeded-data render precondition (the dashboard rendered both cards); no citation needed — CAD-026[0] is owned by dashboard.spec.ts |
| GET `/contacts/overdue` ok; both seeded ids present | KEEP | CAD-023[0] (each entry carries the contact) |
| per-entry `days_overdue >= 1`, `next_due_date` truthy, `suggested_action` non-empty string | KEEP | CAD-023[0] (entry metadata) |
| relative ordering scoped to own two ids (10d before 3d) | KEEP | CAD-023[1] (most-overdue first), correctly scoped against parallel workers |
| (nothing asserts the 1000 bound / truncation) | DROP | CAD-023[2] is not E2E-seedable — waiver W1 (§3) |

Test-level outcome: **keep**; narrow the citation from bare `CAD-023` to `// spec: CAD-023[0], CAD-023[1]` and record waiver W1 for CAD-023[2] in the same PR (a bare cite currently overclaims [2]; narrowing without the waiver would mint a new orphan and trip the scanner once `cadence-followup` flips `e2e_settled: true`).

### 1.9 rematch-on-add-email.spec.ts — `adding an email retroactively links a past calendar event to the contact` (line 27)

Cited today: `// spec: CAL-019` (bare, line 31 — claims all 3 then-items; the test only proves [0]).

| Assertion (short) | Verdict | Notes |
|---|---|---|
| `heading 'Edit Contact'` visible | KEEP | Navigation settle for the edit view; not behavior-proving, carries no citation. Optionally replace with a URL/`getByRole('form')` settle |
| `.group` row scope + `getByRole('combobox').selectOption('email')` + `input[type="email"]` fill | KEEP (rewrite) | Driver only; `.group` is a CSS-class scope and the bare attribute selector is fragile. Rewrite to accessible-name locators once §2 item 2 lands: `getByRole('combobox', { name: … }).first()` / `getByRole('textbox', { name: … })` |
| PUT `/contacts/:id` 200 via exact-pathname `waitForResponse`; `rematch_job_id` truthy | KEEP | This is the browser-only value of the test: the real edit form wired to the rematch dispatch. It observes IMP-019[2]/IMP-021 facts, but both are `surface: api` — do NOT cite them (scanner warns on E2E cites of non-ui behaviors; Go `backend/tests/rematch_api_test.go` already cites IMP-021) |
| poll `GET /rematch/jobs/:id` → status `completed`, `matched >= 1` | KEEP | Job-terminal gate; same IMP-021 note — no citation |
| seeded event id appears in `GET /contacts/:id/events` | KEEP | CAL-019[0] — the contact appended to the matching event's matched set, observed via the calendar read API |

Test-level outcome: **keep + rewrite drivers**; narrow the citation from bare `CAL-019` to `// spec: CAL-019[0]` and record waivers W2/W3 for CAL-019[1]/[2] (§3) in the same PR. Alternative considered and rejected: extending the test to prove [1] (poll for the projected mutual interaction + seed a future event asserting linked-but-no-interaction) would only re-prove through the browser what `backend/tests/rematch_integration_test.go` already owns deterministically.

## 2. Aria surfaces the app must add

No `toHaveClass` assertions exist in these three files, so there are no missing `aria-pressed`/`aria-current` state surfaces here. Two targeted accessible-structure/name gaps force CSS-class drivers and are in scope per the design doc's "targeted a11y additions":

1. **Overdue contact cards have no role container** — `frontend/src/app/dashboard/page.tsx` (Action Required section renders cards as plain `div`s). State/handle needed: list semantics (`<ul role="list">` + `<li>` per card, or `role="listitem"` on the card root) so tests scope a card via `getByRole('listitem').filter({ hasText: name })` instead of `locator('div.rounded-lg')`. Tests that will use it: overdue-contact-updates 1.5 (surviving test), and dashboard.spec.ts's several `div.rounded-lg` scopes (lines ~249/352/435 — child 5's residual-relaxation file; coordinate so the aria lands once).
2. **Contact-form method rows have no accessible names** — `frontend/src/components/contacts/contact-form.tsx` (~line 163): the per-row type `Select` and value `input` carry only ids; the row container is `className="group"`. Add `aria-label` (e.g. "Contact method type" / "Contact method value") or visually-hidden labels. Tests that will use it: rematch-on-add-email 1.9 (replaces `.group` + `input[type="email"]` drivers); contacts.spec.ts edit-flow tests (child 5) benefit identically.

## 3. Waivers to record

| # | Behavior | then | Reason (one line, durable) |
|---|---|---|---|
| W1 | CAD-023 | 2 | the 1000-entry bound and most-overdue-retained truncation cannot be deterministically seeded in an E2E run; the bound is applied in ContactService.ListOverdueContacts and ordering-under-limit is proven by TestContactBy_ListOverdueContacts |
| W2 | CAL-019 | 1 | the past-projects/future-links split is backend projection plumbing with no browser-added coverage; proven deterministically by TestRematch_CalendarPastEvent_RecordsInteraction and TestRematch_CalendarFutureEvent_NoInteraction |
| W3 | CAL-019 | 2 | not re-emitting already-projected attendees is a negative with no UI surface; proven by TestRematch_CalendarIdempotent and rematch_event_dedup_test.go |

All three waivers are companions to citation-narrowing in §1.8/§1.9 — land waiver + narrowed cite in the same PR so the scanner never sees a transient orphan.

## 4. Coverage gaps (backfill list)

**cadence-followup (CAD): 0 orphans in the scanner snapshot.** The only new orphan this work-list creates is CAD-023[2], handled by waiver W1.

**ingest (ING): zero `surface: ui` then-items** — nothing to cover; deleting gchat-contact-signal.spec.ts costs nothing.

**Gap G1 — missing behavior revealed by test 1.5 (this file set's one genuine gap):** the contact-detail-page "Log Interaction" modal flow has no SSOT behavior (dashboard variant = CAD-028, list-row variant = CON-044, detail variant = none). Per the design doc, file it: mint in `spec/contacts.yaml` (suggested id `CON-053`, next free), `type: ux`, `status: current`, `surface: ui`, given a contact detail page, when the user logs an interaction via the Log Interaction modal, then roughly: [0] an interaction is logged with the chosen direction (defaulting to mutual), timestamped by the server; [1] an overdue contact leaves the overdue list once cadence recomputes. Covering test: the rewritten 1.5 cites `CON-053[0], CON-053[1]`. Run `make spec-lint`.

**calendar (CAL): 19 orphans (CAL-024..CAL-030), none provable by this file set** — recorded here because CAL is one of my assigned domains, with dispositions pointing at the owning files/children:

| Orphan | Disposition |
|---|---|
| CAL-024[0..1] (Meetings section shown iff events) | (a) existing tests to relax+cite: meetings.spec.ts `should display meetings section…` (CAL-024[0]) and `should not show meetings section when no events exist` (CAL-024[1]) — owned by the notes-meetings child (meetings.spec.ts is its file) |
| CAL-025[0..3] (all/upcoming/past filters, counts, default, past ordering) | (a) meetings.spec.ts filter tests (`should filter by upcoming events`, `should filter by past events`) cover [0..2] after relax; [3] (past most-recent-first) may need one added assertion over seeded dates — notes-meetings child |
| CAL-026[0..3] (card contents; past de-emphasis) | (a) meetings.spec.ts display test partially covers [0..2] (seeded title/date/location/count are data assertions); [3] "visually de-emphasized" has no data/aria surface — candidate for that child's judge/DROP call, or the domain's visual-guard budget |
| CAL-027[0..1] (source-link opens externally / plain text without) | (a) meetings.spec.ts `should display html_link as clickable external link` covers [0]; [1] needs a no-link seeded event assertion — notes-meetings child |
| CAL-028[0..2] (progressive reveal) | (a) meetings.spec.ts `should show load more button when many events exist` covers [0] and partially [1]; [2] (filter switch resets reveal) needs one added step — notes-meetings child |
| CAL-029[0..1] (per-account calendar sync control + state polling) | (a) settings.spec.ts / route-mocked provider states — settings child (sanctioned route-mock KEEP technique) |
| CAL-030[0..1] (staleness banner names Google Calendar) | (a) settings.spec.ts staleness coverage — settings child; backend staleness API already has Go tests (`staleness_api_test.go`) |

**imports-matching (IMP): 21 orphans (IMP-026..IMP-031), out of scope for this file set** — after mapping, none of my three files proves any IMP behavior (the rematch test observes IMP-019/IMP-021, both `surface: api`, Go-cited in `rematch_api_test.go`). All 21 orphans are the import review page/modal surfaces exercised by the nine `imports-*.spec.ts` files, owned by the imports child (design-doc child 2). No action from this work-list; flagged so the parent can confirm the imports child's list covers them.

## 5. Citations to add to existing tests

| Test | Citation change |
|---|---|
| overdue-contact-updates 1.6 `all views should show consistent state…` | keep `CAD-028[2]`; ADD `CAD-029[1]` on the detail-page `Last response:` assertion (additive N:M — contact-direction.spec.ts keeps its cites) |
| overdue-contact-updates 1.8 `should show multiple overdue contacts…` | REPLACE bare `// spec: CAD-023` with `// spec: CAD-023[0], CAD-023[1]` + waiver W1 |
| overdue-contact-updates 1.5 (detail-page Log Interaction) | ADD `// spec: CON-053[0], CON-053[1]` once G1 is minted |
| rematch-on-add-email 1.9 | REPLACE bare `// spec: CAL-019` with `// spec: CAL-019[0]` + waivers W2/W3; do NOT add IMP-019/IMP-021 cites (api-surface — scanner warns) |
| gchat-contact-signal (both tests) | none — file deleted; CAD-029[0]/[1] remain cited by contact-direction.spec.ts |

## Execution summary for the implementing agent

1. Delete `gchat-contact-signal.spec.ts`; remove its `test-map.json` entry.
2. In `overdue-contact-updates.spec.ts`: delete tests 1.3, 1.4, 1.7; rewrite 1.5 (drop copy assertion, pin POST direction, cite CON-053 after minting it in `spec/contacts.yaml`); light-rewrite 1.6 (header-derived column index, add CAD-029[1] cite); narrow 1.8's cite + add waiver W1 to `spec/cadence-followup.yaml`.
3. In `rematch-on-add-email.spec.ts`: rewrite drivers to accessible-name locators (needs §2 item 2 aria-labels in `contact-form.tsx`); narrow cite to `CAL-019[0]` + add waivers W2/W3 to `spec/calendar.yaml`.
4. Frontend a11y (in scope): §2 items 1 (dashboard card list semantics — coordinate with child 5's dashboard.spec.ts relaxation) and 2 (contact-form method-row labels).
5. Run `make spec-lint && make spec-coverage` — expect no new orphans (waivers land with the narrowed cites) and 0 invalid citations; run `make test-e2e-diff`.
