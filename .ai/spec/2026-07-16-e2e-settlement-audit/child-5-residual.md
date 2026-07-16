# Child 5 work-list: contacts/dashboard/cadence residual relaxation

Audit date: 2026-07-16. Scope: the nine already-cited (but not systematically relaxed) spec files from the #659–661 verifier→E2E migration: `contacts.spec.ts`, `contact-navigation.spec.ts`, `contact-direction.spec.ts`, `contact-merge.spec.ts`, `contact-tasks.spec.ts`, `dashboard.spec.ts`, `birthdays.spec.ts`, `navigation.spec.ts`, `error-boundary.spec.ts` (all under `frontend/tests/e2e/`). Domains: contacts (CON), dashboard (DSH), cadence-followup (CAD). Scanner state at audit time: CON 22 covered / 1 orphan (CON-042[0]); DSH 11 covered / 3 waived (DSH-005[1..3]) / 1 orphan (DSH-004[2]); CAD 25 covered / 0 orphaned.

Summary of test-level outcomes across the 96 `test(` blocks: 75 KEEP (≈50 as-is or citation-only, ≈25 needing a rewrite/trim — driver fixes, class→aria after section 2, or assertion trims), 21 delete-test (19 pure DELETE verdicts + 2 MOVE verdicts whose Go coverage already exists). 0 new waivers proposed — both scanner orphans are deterministically backfillable (section 4). Visual-guard budget: CON 2/2 (leading-normal, context-menu clipping — both need the explicit visual-guard comment added), CAD 0/2 and DSH 0/2 if the section-2 aria additions land (fallback: CAD 2/2, DSH 1/2).

A recurring finding: several tests prove real user-facing flows that have NO current `surface: ui` behavior to cite (Log Interaction modal, create form, column sorting, pagination, cadence filter, nav-bar buttons, context preservation). Per the design doc these "reveal a missing behavior — file it in spec/". Proposed mints are collected in section 4b; the implementation PR should mint them as `status: current` (they describe today's working behavior) and cite them, rather than leaving settled tests uncited.

---

## 1. Per-test triage

### 1.1 contacts.spec.ts

#### `should use leading-normal on contact name to prevent descender clipping` (L23)

| Assertion | Verdict | Notes |
|---|---|---|
| `toHaveClass(/leading-normal/)` + `not.toHaveClass(/leading-7/)` | KEEP (visual guard) | Real already-bitten bug (descender clipping, `.ai/rules/core.md` gotcha row); no data/aria surface. CON visual-guard 1/2. Add the explicit `// visual-guard:` comment the design doc requires. No citation (no behavior). |

Outcome: keep as explicitly-commented visual guard; add comment.

#### `should truncate long location with tooltip in contacts table` (L46)

| Assertion | Verdict | Notes |
|---|---|---|
| row visible (seeded name) | — | driver only |
| `toHaveAttribute('title', longLocation)` | DELETE | tooltip mechanics = presentation; no SSOT behavior |
| `span.truncate` visible | DELETE | pure CSS/DOM structure |
| detail-page heading visible after goto | DELETE | proves nothing beyond other tests |

Outcome: delete test. Truncation/tooltip quality is judge-owned (Track B). If the maintainer wants it pinned, that's a new CON ux behavior — not recommended.

#### `should show expandable notes for long content` (L86)

| Assertion | Verdict | Notes |
|---|---|---|
| seeded note text visible | KEEP | seeded-data-in-place |
| `Show more` button visible → click → `Show less` → click → `Show more` | KEEP | role-based, proves the clamp/expand/collapse control appears on overflow — this is NTS-007[2] |

Outcome: keep as-is; add `// spec: NTS-007[2]` (cross-domain cite; coordinate with child 4, which owns notes-meetings, so it doesn't author a duplicate).

#### `should not show expand button for short notes` (L132)

| Assertion | Verdict | Notes |
|---|---|---|
| short note visible | KEEP | seeded data |
| `Show more` not visible | KEEP | the only-when-overflowing half of NTS-007[2] |

Outcome: keep as-is; add `// spec: NTS-007[2]` (coordinate with child 4).

#### `should log a backdated interaction via the Log Interaction modal` (L158)

| Assertion | Verdict | Notes |
|---|---|---|
| dialog visible after `Log Interaction` click | KEEP | role-based |
| POST /interactions 201 + `body.success` | KEEP | API response assertion |
| dialog closes on success | KEEP | |

Outcome: keep; cite the proposed mint CON-A "Log Interaction modal" (section 4b) — currently uncited.

#### `should show context menu without clipping for bottom rows` (L197)

| Assertion | Verdict | Notes |
|---|---|---|
| last row's menu items visible (menuitem roles) | KEEP (visual guard) | guards the already-bitten context-menu clipping bug (`.ai/rules/core.md` gotcha). CON visual-guard 2/2. Add explicit `// visual-guard:` comment. No citation. |

Outcome: keep as explicitly-commented visual guard; add comment. (The Edit/Merge menuitem-presence asserts are part of the same clipping proof — fine.)

#### `should navigate to edit mode via context menu Edit action` (L224) — cited CON-041[0], CON-041[1]

| Assertion | Verdict | Notes |
|---|---|---|
| `waitForURL(/action=edit/)` | KEEP | route state |
| heading `Edit Contact` visible | KEEP | mode-entry anchor (edit mode's observable), not copy regression |
| `not.toHaveURL(/action=/)` | KEEP | CON-041[1] param-strip |

Outcome: keep as-is; already compliant.

#### `should navigate to merge modal via context menu Merge action` (L254) — cited CON-041[0], CON-041[1]

Same shape as above (merge variant). Outcome: keep as-is.

#### `should log a mutual interaction via the Log Interaction modal default` (L284)

| Assertion | Verdict | Notes |
|---|---|---|
| POST 201, `data.direction === 'mutual'` | KEEP | proves the modal default direction — cite mint CON-A |
| dialog closes | KEEP | |
| API refetch: `last_response_at`/`last_outreach_at` truthy | MOVE | re-checks CAD-006 (surface: none) through the browser; Go coverage exists (`internal/consumer/cadence_updater_test.go`, `tests/api/direction_api_test.go: TestContactAPI_DirectionTimestamps`). Delete these assertions. |

Outcome: rewrite (drop the timestamp refetch block); cite mint CON-A.

#### `should log an outbound interaction via the Log Interaction modal` (L328)

| Assertion | Verdict | Notes |
|---|---|---|
| direction picker click + POST 201 `direction === 'outbound'` | KEEP | modal direction choice reaches the API — cite mint CON-A |
| before/after API fetches: `last_outreach_at` truthy, `last_contacted` unchanged | MOVE | CAD-006[0] semantics, Go-covered (same targets as above). Delete. |

Outcome: rewrite; cite mint CON-A.

#### `should log an inbound interaction via the Log Interaction modal` (L378)

Same shape: KEEP the picker + POST `direction === 'inbound'` assertion (cite mint CON-A); MOVE the `last_response_at`/`last_outreach_at` refetch assertions (CAD-006[1], Go-covered). Outcome: rewrite.

#### `defaults the contact list to cadence order, most-frequent-first` (L423) — cited CON-038[0]

| Assertion | Verdict | Notes |
|---|---|---|
| bare-load list request carries `sort=cadence`-or-default | KEEP | network-param assertion |
| seeded rows render weekly→monthly→annual in DOM order | KEEP | ordering IS the then-item; seeded-data ordering is data-asserting, not visual-copy |

Outcome: keep as-is; already compliant.

#### `deletes a contact only after confirmation, then returns to the list` (L471) — cited CON-042[1], CON-042[2]

| Assertion | Verdict | Notes |
|---|---|---|
| dismiss path: no DELETE fired (bounded window) + contact still 200 | KEEP | CON-042[1] "only on confirmation" |
| accept path: DELETE 204 | KEEP | |
| `toHaveURL(/\/contacts(\?|$)/)` | KEEP | CON-042[2] |
| contact 404 after | KEEP | DB-state via API |

Outcome: keep; EXTEND to close orphan CON-042[0] — capture `dialog.message()` in the dismiss-path handler and assert it contains `cannot be undone` (copy verified at `src/app/contacts/[id]/page.tsx:213`), add `// spec: CON-042[0]`. Same technique as contact-tasks' unlink confirmMessage precedent.

#### `logs a mutual interaction from the list-row Mark as Contacted quick action` (L525) — cited CON-044[0]

All assertions (POST 201, no client `occurred_at`, `direction === 'mutual'`, server `occurred_at` truthy) KEEP — network/data assertions. Outcome: keep as-is.

#### `should filter contacts by cadence status` (L576) — cited DSH-007[0]

| Assertion | Verdict | Notes |
|---|---|---|
| two-phase search: `search=` param + matching rows appear, non-matching disappears | KEEP | cited DSH-007[0]; network-param + seeded data |
| `getByLabel('Filter by cadence')` `toHaveValue('')` | KEEP | labeled control, value state |
| `cadence_filter=has_cadence` / `no_cadence` responses + row visibility flips | KEEP | network-param + seeded data — cite mint CON-B (list filter) |
| reset to all | KEEP | |

Outcome: keep; add mint CON-B citation for the filter half (currently rides uncited).

#### `should create a contact from the form @smoke` (L691)

| Assertion | Verdict | Notes |
|---|---|---|
| fill Full Name → `waitForURL(/\/contacts\/[id]$/)` → heading visible | KEEP | route + seeded-data; the core create flow |

Outcome: keep; cite mint CON-C (create-from-form). Keep `@smoke`.

#### `should edit contact notes` (L705)

| Assertion | Verdict | Notes |
|---|---|---|
| seeded note visible; edit → fill Notes → Update Contact → updated note visible, old absent | KEEP | proves the displayed notepad reflects the new content after save — NTS-008[2] |

Outcome: keep as-is; add `// spec: NTS-008[2]` (coordinate with child 4 — it may also want NTS-008[0]/[1] proofs here; the two-operations and clear-removes-note facts are NOT proved by this test).

#### `should display contact with all methods and normalized handles` (L742)

| Assertion | Verdict | Notes |
|---|---|---|
| each method's normalized value visible (`@@x` → `@x`) | MOVE (mostly) | normalization is CON-012 (surface: none), Go-covered (`internal/identity/normalize_test.go`). Keep at most one seeded-method-visible spot check as display proof under mint CON-D. |
| raw `@@handle` has count 0 | MOVE | same |
| `Primary` badge on the Telegram row, count 1 | KEEP | the one-primary display fact — cite mint CON-D |
| `Google Chat` label visible | DELETE | static type-label copy |
| gchat value not a link (`link` count 0) | DELETE | DOM structure |

Outcome: rewrite to a slim seeded-methods-display + primary-badge test citing mint CON-D, or delete entirely if the maintainer declines the mint. Note: `.locator('..')` parent-walk driver should become a row-scoped role/region locator during rewrite.

#### `should display cadence column with formatted values and sort by frequency` (L799)

| Assertion | Verdict | Notes |
|---|---|---|
| `th` Cadence header visible | DELETE | static header copy |
| `Weekly`/`Monthly`/`Annual` `.first()` visible | DELETE | unscoped `.first()` over shared DB — proves nothing about our rows; formatted-label copy |
| click header + `networkidle` + `svg` visible | DELETE | DOM structure; sort round-trip covered by the contact_by/last_response_at sort tests, cadence-order default covered by CON-038[0] test |

Outcome: delete test (fully redundant + brittle; CON-020 semantics are surface: none, Go-owned).

#### `should display Next Contact column header and render dates` (L838)

| Assertion | Verdict | Notes |
|---|---|---|
| columnheader `Next Contact` visible | DELETE | static copy |
| `cells.nth(5)` not `-` / is `-` | DELETE | positional DOM (column-index) assertion of CON-001[1]/[2] facts (surface: none, backend-owned) |

Outcome: delete test.

#### `should sort by Next Contact column when header clicked` (L885)

| Assertion | Verdict | Notes |
|---|---|---|
| header click → `sort=contact_by&order=asc` response, click → `order=desc` | KEEP | network-param round trip — cite mint CON-E (column sorting) |
| `svg` visible | DELETE | DOM structure |
| contact still visible after toggles | KEEP | cheap liveness anchor |

Outcome: rewrite (drop svg assert); cite mint CON-E.

#### `should display Last response column header and sort by last_response_at` (L925)

| Assertion | Verdict | Notes |
|---|---|---|
| `Last response` header visible; legacy `Last Contact` count 0 | DELETE | static header copy either way |
| click → `sort=last_response_at&order=desc` then `asc` responses | KEEP | cite mint CON-E |
| `svg` visible | DELETE | DOM structure |

Outcome: rewrite; cite mint CON-E. (Consider merging this and the previous test into one column-sorting test during implementation.)

#### `should show page number buttons and top/bottom pagination when multiple pages exist` (L967)

| Assertion | Verdict | Notes |
|---|---|---|
| `[data-testid="pagination"]` count 2 | DELETE | DOM-structure (top/bottom duplication is presentation) |
| page `1`/`2` buttons visible | KEEP | pagination controls exist — cite mint CON-F |
| `toHaveClass(/bg-blue-600/)` ×4 (active page) | KEEP after rewrite | rewrite to `toHaveAttribute('aria-current', 'page')` — `src/components/ui/pagination.tsx:64` ALREADY sets aria-current; no app change needed |
| `Previous` enabled / `Next` disabled at last page; click Previous → page 1 active | KEEP | aria/disabled state |
| bottom pagination shows page 1 active | KEEP after rewrite | same aria-current rewrite; proves the two controls stay in sync |

Outcome: rewrite (class → aria-current, drop count-2); cite mint CON-F.

### 1.2 contact-navigation.spec.ts

#### `should navigate between contacts with arrow keys @smoke` (L22)

| Assertion | Verdict | Notes |
|---|---|---|
| prev/next buttons visible; ArrowRight → URL contains `/contacts/` | DELETE | vacuous (any contact URL passes); fully subsumed by the CON-040[0] test at L409 |

Outcome: delete test; move the `@smoke` tag to `arrow keys move to the previous/next contact and disable at both boundaries` (L409).

#### `should show navigation bar with position indicator` (L58)

| Assertion | Verdict | Notes |
|---|---|---|
| `/\d+ of \d+/` visible; prev/next buttons visible | DELETE | the indicator is already exercised as the readiness gate inside four cited tests; standalone presence proves nothing extra |

Outcome: delete test. If mint CON-G (nav-bar buttons) gains a position-indicator then-item, assert it inside that cited test instead.

#### `should disable navigation at boundaries` (L89)

| Assertion | Verdict | Notes |
|---|---|---|
| Previous disabled at first, Next enabled | DELETE | subsumed by L409 (CON-040[0]) which proves BOTH boundaries plus actual movement |

Outcome: delete test.

#### `should disable keyboard navigation in edit mode` (L116) — cited CON-040[1]

All assertions KEEP (readiness gate, `disabled` attribute on both buttons, pre-registered nav probe, URL unchanged). Outcome: keep as-is; compliant.

#### `should not navigate when typing in input fields` (L170) — cited CON-040[1]

All assertions KEEP (nav probe + URL). Outcome: keep as-is.

#### `should preserve URL context (sort, search) during navigation` (L217)

| Assertion | Verdict | Notes |
|---|---|---|
| URL contains `sort=name`, `order=asc` before and after ArrowRight | KEEP | route-state assertion of context carrying |

Outcome: keep; cite mint CON-H (context carried across detail navigation — the current-status half of proposed CON-039; see section 4b). Currently uncited and un-citable (CON-039 is `proposed`, citing it is forbidden).

#### `should navigate via navigation bar buttons` (L247)

| Assertion | Verdict | Notes |
|---|---|---|
| Next click → URL changed, contains `/contacts/` | KEEP after rewrite | weak: assert the ACTUAL adjacent contact (seed name-asc order, expect ids[1]) like the L409 test does |

Outcome: rewrite; cite mint CON-G (on-screen prev/next buttons — CON-040 pins arrows only; do NOT extend CON-040's then list, which would shift indexes for its four citing tests).

#### `should handle Escape key to return to list` (L280) — cited CON-040[3]

| Assertion | Verdict | Notes |
|---|---|---|
| Escape → heading `Contacts` visible | DELETE | subsumed by the restore-state test below (same citation, strictly stronger) |

Outcome: delete test.

#### `should restore search and sort state after Escape back to list` (L305) — cited CON-040[3]

All assertions KEEP (URL params restored, search input value restored; `Contacts` heading is a settle anchor). Outcome: keep as-is.

#### `detail prev/next follows the same default (cadence) ordering as the list` (L343) — cited CON-038[1]

All assertions KEEP (ids_only request params, URL walk weekly→monthly→annual, boundary disabled states). Outcome: keep as-is; exemplary.

#### `arrow keys move to the previous/next contact and disable at both boundaries` (L409) — cited CON-040[0]

All assertions KEEP. Outcome: keep as-is; add `@smoke` (transferred from L22).

#### `Enter opens edit mode when focus is outside an input` (L463) — cited CON-040[2]

KEEP (heading `Edit Contact` as mode anchor). Outcome: keep as-is.

#### `Escape discards an unsaved edit without persisting the change` (L482) — cited CON-040[3]

All assertions KEEP (no PUT/PATCH fired, read view restored, stored name unchanged via API). Outcome: keep as-is.

#### `arrows are inert while focus is in an input outside edit mode` (L533) — cited CON-040[1]

All assertions KEEP (pre-registered nav probe, URL unchanged). Outcome: keep as-is.

### 1.3 contact-direction.spec.ts

#### `shows direction signal timestamps after mutual interaction` (L22) — cited CAD-029[0], CAD-029[1]

KEEP: `/Last outreach: \S+/`, `/Last response: \S+/` — dynamic-value regexes (data-asserting; the label is the anchor, the `\S+` is the datum). Outcome: keep as-is.

#### `shows outreach but not response after outbound-only interaction` (L52) — cited CAD-029[0], CAD-029[1]

KEEP (shown-iff-exists positive + negative). Outcome: keep as-is.

#### `shows awaiting-reply indicator while a follow-up pends` (L85) — cited CAD-029[2]

KEEP — fetch-and-patch route mock is the sanctioned technique; `getByTitle('Awaiting reply')` is an accessible-name driver. Outcome: keep as-is.

#### `shows explicit no-recent-activity state with no direction signals` (L126) — cited CAD-029[3]

KEEP — `No recent activity` is the explicit-state observable the then-item pins; negatives for the three signals. Outcome: keep as-is.

#### `interaction API response includes direction field` (L146)

| Assertion | Verdict | Notes |
|---|---|---|
| POST/list interactions carry `direction` | MOVE | no page involved; already covered by `backend/tests/api/direction_api_test.go: TestInteractionAPI_DirectionInResponse` |

Outcome: delete test (Go coverage exists; nothing to author).

#### `contact API response includes new direction timestamp fields` (L174)

| Assertion | Verdict | Notes |
|---|---|---|
| contact GET has `last_interaction_at`/`last_outreach_at`/`last_response_at`/`has_pending_followup` | MOVE | covered by `TestContactAPI_DirectionTimestamps` + `TestContactAPI_HasPendingFollowup` in the same Go file |

Outcome: delete test.

### 1.4 contact-merge.spec.ts

Cross-cutting driver rot in this file: (a) `page.locator('.fixed.inset-0')` as the modal locator — CSS-class driver; requires the section-2 `role="dialog"` addition, then `getByRole('dialog', { name: 'Merge Contacts' })`; (b) `[class*="cursor-pointer"]` / `[class*="select-none"]` as dropdown-option locators — requires the section-2 option-role work on `contact-selector.tsx`, then `getByRole('option', { name: sourceName })`; (c) `modal.locator('h3')` — becomes `getByRole('heading', { level: 3 })`. Every KEEP below inherits these rewrites.

#### `should open merge modal from action menu` (L45)

| Assertion | Verdict | Notes |
|---|---|---|
| `Merge Contacts` heading + `Keeping` + `Archiving` text | DELETE | static copy; modal-opens is proved by every other test in the file, and Keeping is cited at L72 |

Outcome: delete test.

#### `should display target contact as "Keeping"` (L72) — cited CON-043[0]

| Assertion | Verdict | Notes |
|---|---|---|
| modal contains target name | KEEP | seeded data in the kept slot |
| `Keeping` badge visible | KEEP | the "marked as kept" observable; scope it to the target's region during rewrite |

Outcome: rewrite drivers (dialog role); keep citation.

#### `should search and select source contact` (L98) — cited CON-043[0], CON-043[1]

| Assertion | Verdict | Notes |
|---|---|---|
| search → source option appears | KEEP | seeded data; rewrite to `getByRole('option')` |
| target absent from options (`select-none` count 0) | KEEP | the excludes-target clause; rewrite to option role |
| `Will Be Merged` appears after select | KEEP | preview-loaded anchor (CON-043[1]) |

Outcome: rewrite drivers; keep citations.

#### `should show field conflicts when contacts have different values` (L149)

| Assertion | Verdict | Notes |
|---|---|---|
| `hasConflicts ? assert buttons : assert preview` conditional | DELETE | either-branch-passes conditional is vacuous; the toggle test (L203) proves conflicts deterministically |

Outcome: delete test.

#### `should toggle field selection between source and target` (L203) — cited CON-043[2]

| Assertion | Verdict | Notes |
|---|---|---|
| `Resolve Conflicts` visible | KEEP | conflict-section anchor |
| default keeps target: `toHaveClass(/bg-blue-600/)` on Monthly/New York/target-birthday | KEEP after rewrite | selection state → `aria-pressed` (section 2: merge-modal FieldToggle); assert `toHaveAttribute('aria-pressed', 'true'/'false')` |
| each field toggles independently (6 more class asserts) | KEEP after rewrite | same aria-pressed rewrite |

Outcome: rewrite (class → aria-pressed after frontend addition); keep citation. This is the file's flagship residual `toHaveClass`.

#### `should edit merged contact name` (L283) — cited CON-043[3]

| Assertion | Verdict | Notes |
|---|---|---|
| click name → text input appears → fill + Enter → h3 shows new name | KEEP | editable-name flow; rewrite `h3`/`input[type="text"]` drivers to roles (heading/textbox) |

Outcome: rewrite drivers; keep citation.

#### `should cancel name edit with Escape` (L322)

| Assertion | Verdict | Notes |
|---|---|---|
| Escape restores original name, typed name not visible | KEEP | the editable-name contract's discard path (no accidental rename) |

Outcome: rewrite drivers; add `// spec: CON-043[3]`.

#### `should close modal when pressing Escape` (L357)

| Assertion | Verdict | Notes |
|---|---|---|
| Escape → modal heading gone | DELETE | generic modal dismissal; no CON-043 then-item; judge-owned interaction quality |

Outcome: delete test.

#### `should close modal when clicking backdrop` (L383)

Same as above. Outcome: delete test.

#### `should successfully merge contacts` (L409) — cited CON-043[5]

| Assertion | Verdict | Notes |
|---|---|---|
| merge POST observed, status==200 else throw | KEEP | API response |
| `/merged successfully/i` visible then auto-dismissed | KEEP | CON-043[5] "reported and auto-dismissed" — the outcome banner is the observable |
| modal closed | KEEP | |
| transferred phone + note visible on target | KEEP | seeded-data-in-place post-flow (thin; CON-027/CON-028 semantics stay Go-owned) |

Outcome: rewrite drivers only; keep citation. (Simplify the `if (responseStatus === 200)` block to a straight `expect(response.status()).toBe(200)`.)

#### `should show quick-fill name option when source has different name` (L493) — cited CON-043[3]

| Assertion | Verdict | Notes |
|---|---|---|
| `use this` visible → click → h3 shows source name | KEEP | the quick-fill clause; rewrite `use this` to a role-based locator (it should be a button — see section 2 note) |

Outcome: rewrite drivers; keep citation.

#### `should disable merge button when no source selected` (L538) — cited CON-043[4]

KEEP (`toBeDisabled`). Outcome: keep as-is.

#### `should show loading state during merge` (L563)

| Assertion | Verdict | Notes |
|---|---|---|
| merge POST + success toast | DELETE | despite the name it never asserts a loading state; strict subset of L409 and of the in-flight test (L665) |

Outcome: delete test.

#### `keeps the merge submit disabled while the preview is loading` (L625) — cited CON-043[4]

KEEP (route-delayed preview + `toBeDisabled` → `toBeEnabled`). Outcome: rewrite drivers; keep citation.

#### `keeps the merge submit disabled while the merge is in flight` (L665) — cited CON-043[4]

KEEP (route-delayed POST + disabled `Merging` button + completion). The `Merging` accessible-name change is the in-flight observable — acceptable. Outcome: rewrite drivers; keep citation.

### 1.5 contact-tasks.spec.ts

This file was already reworked (#657) and is the closest to settled; route-mocking here is the sanctioned provider-boundary technique.

#### `shows tasks section on contact detail page` (L103) — cited CAD-030[3]

| Assertion | Verdict | Notes |
|---|---|---|
| `Tasks` heading + `Add` button visible | KEEP | role-based |
| `No tasks yet` visible | KEEP | the empty-state observable |
| `Add a task to track follow-ups for this contact` visible | DELETE | second static-copy sentence adds only copy-brittleness; the invite is proved by `No tasks yet` + the Add button |

Outcome: trim one assertion; keep citation.

#### `lists follow-up tasks first with a distinct pending indicator, then manual tasks` (L129) — cited CAD-030[0]

| Assertion | Verdict | Notes |
|---|---|---|
| rows count 2, follow-up row before manual row | KEEP | ordering is the then-item; mocked data DOM order |
| `svg.text-amber-400` present/absent | KEEP after rewrite | pending-indicator state has no aria surface — add `aria-label="Awaiting reply"` to the Clock icon (section 2), then assert `getByLabel`/`getByTitle`. Fallback: CAD visual-guard 1/2 with explicit comment. |
| `tasksSection()` helper uses `div.bg-white.shadow` | rewrite | see section 2 (region semantics for the Tasks card); applies to all tests using the helper |

Outcome: rewrite after aria additions; keep citation.

#### `derives each task badge from its kind and lifecycle` (L166) — cited CAD-030[1]

KEEP — badge labels are DERIVED from kind/lifecycle data (the mapping is the behavior), asserted per-row exact. Outcome: keep (helper rewrite only).

#### `collapses completed tasks behind a toggle with a count` (L220) — cited CAD-030[2]

KEEP — `Show completed (2)` / `Hide completed (2)` is a count-in-label assertion, but the count IS the then-item ("a toggle with a count") over mocked data; justified, not the DELETE smell. Hidden→visible flip is state. Outcome: keep as-is.

#### `opens add task modal and closes it with Escape` (L251) — cited CAD-031[0]

KEEP — already the aria-pressed exemplar (kind segmented control). Outcome: keep as-is.

#### `disables submit while task text is empty` (L305) — cited CAD-031[1]

KEEP (`toBeDisabled`/`toBeEnabled`, optional-notes affordance). Outcome: keep as-is.

#### `created task appears in the live tasks list` (L332) — cited CAD-031[2]

KEEP (mocked write loop, POST body asserted, created row renders). Outcome: keep (helper rewrite only).

#### `offers unlink with confirmation and fires only the CRM-link DELETE` (L394) — cited CAD-033[0]

KEEP — DELETE 204, URL contains task id, confirm message captured and asserted (`remain in Todoist` — the confirmation's substance, the CON-042[0] precedent), row leaves. `button[title=...]` is an accessible-name driver (title) — acceptable; may switch to aria-label during the section-2 pass. Outcome: keep.

#### `dismissing the unlink confirmation leaves the task linked` (L451) — cited CAD-033[0]

KEEP (bounded absence window for the DELETE). Outcome: keep.

#### `exposes no in-CRM complete or dismiss control on a linked task` (L488) — cited CAD-033[1]

KEEP (control-inventory negatives via roles/titles). Outcome: keep.

### 1.6 dashboard.spec.ts

#### `should display dashboard with navigation @smoke` (L36) — cited DSH-001[0]

| Assertion | Verdict | Notes |
|---|---|---|
| `toHaveURL('/dashboard')` after `/` | KEEP | the cited then-item |
| `toHaveTitle(/Personal CRM/)` | DELETE | static copy |
| Dashboard/Contacts links visible | DELETE | duplicated by navigation.spec.ts DSH-002[0] loop |
| `Action Required` h2 visible | KEEP | render-settled anchor for the redirect target |

Outcome: rewrite (trim); keep citation + `@smoke`.

#### `should navigate to contacts from dashboard` (L57)

| Assertion | Verdict | Notes |
|---|---|---|
| click Contacts link → URL + heading | DELETE | DSH-002[0] asserts all five links' hrefs on every surface; link-follows-href is browser behavior |

Outcome: delete test.

#### `should show dashboard content when loaded` (L69)

| Assertion | Verdict | Notes |
|---|---|---|
| `hasOverdue \|\| hasCaughtUp` | DELETE | either-branch conditional (vacuous); both states are deterministically proved by cited tests (CAD-026[0]/[2]) |

Outcome: delete test.

#### `caught-up state offers add-contact and view-list paths` (L88) — cited DSH-003[0], DSH-003[1]

KEEP (route-mocked empty overdue; link hrefs asserted; header CTA helper). Outcome: keep as-is.

#### `dashboard exposes no dashboard-level or global search surface` (L117) — cited DSH-007[1]

KEEP (settled-state-first negative existence proof via roles/placeholder). Outcome: keep as-is.

#### `shows overdue contacts as cards with the count in the header` (L154) — cited CAD-026[0]

| Assertion | Verdict | Notes |
|---|---|---|
| each seeded card heading visible | KEEP | seeded data |
| header==cards single-DOM-pass poll | KEEP | count-in-header IS the then-item; the invariant framing is parallel-safe. The `document.querySelectorAll('p')` scan is a structural driver — acceptable inside `page.evaluate`, optionally tightened when the header gains a stable role/testid. |

Outcome: keep as-is.

#### `shows the all-caught-up state when nothing is overdue` (L192) — cited CAD-026[2]

| Assertion | Verdict | Notes |
|---|---|---|
| `All caught up! 🎉` heading | KEEP | the state's observable anchor |
| `You're all caught up` text | DELETE | second copy assertion of the same state |

Outcome: trim one; keep citation.

#### `each card shows urgency tier, cadence, recency, a reachable method, and the suggested action` (L222) — cited CAD-026[1]

| Assertion | Verdict | Notes |
|---|---|---|
| tier dot `div.rounded-full.bg-{yellow,orange,red}-500` per boundary case | KEEP after rewrite | tier state has no aria surface — add `aria-label`/`title` naming the tier on the dot (section 2), then assert by accessible name. Fallback: CAD visual-guard 2/2 (already explicitly commented). |
| `(weekly cadence)` text | KEEP | mocked-data-derived |
| `N days overdue - Last contacted \S+` regex | KEEP | data + non-empty value |
| method email + `Email` chip | KEEP | mocked data (the `Email` type chip is derived from method type — data-adjacent, fine) |
| 💡 suggestion non-empty | KEEP | data presence |
| `div.rounded-lg` card locator | rewrite | scope cards via heading-anchored region or a `data-testid`/role addition (section 2, optional) |

Outcome: rewrite after aria addition; keep citation.

#### `urgency (default) orders most-overdue first` (L313) — cited CAD-027[0]

KEEP — DOM order of mocked cards is the only observable for a client-side sort; ordering is the then-item. `h3` scan → `getByRole('heading', level 3)` during touch-ups. Outcome: keep.

#### `name orders alphabetically` (L324) — cited CAD-027[1]

KEEP. Outcome: keep.

#### `last-contacted orders oldest first with never-contacted last` (L338) — cited CAD-027[2]

KEEP (incl. `Never contacted` null-sink observable). Outcome: keep.

#### `header add-contact action is available on a populated dashboard` (L390) — cited DSH-003[0]

KEEP (state-first, then CTA helper with href). Outcome: keep as-is.

#### `marking contact as contacted updates dashboard immediately without navigation` (L403) — cited DSH-005[0], CAD-028[0], CAD-028[1]

| Assertion | Verdict | Notes |
|---|---|---|
| POST mutual + server-assigned occurred_at inside click bracket, sub-second precision | KEEP | CAD-028[0] |
| content-predicate overdue refetch (id absent) + card gone + still on /dashboard | KEEP | CAD-028[1], DSH-005[0] |
| header==cards poll with sentinel contact | KEEP | count-updates clause |
| TWO window sentinels (`__dsh005NoReload` and `__noReloadSentinel`) | KEEP one | duplicate mechanism; delete one sentinel pair during touch-up |

Outcome: keep; minor trim. CAD-028[2] deliberately lives in overdue-contact-updates.spec.ts (not this child's file set) — no action.

### 1.7 birthdays.spec.ts

#### `shows placeholder-year birthdays without age and keeps today at the top` (L84) — cited CON-045[3]

| Assertion | Verdict | Notes |
|---|---|---|
| list row shows `M/D`, not `/00`, not `1900` | KEEP | placeholder-year suppression, data-derived |
| detail shows short date, `1900` absent | KEEP | also proves KNW-035[0]/[1] on the detail surface — add cites (section 5) |
| birthdays card: long date, `Today!`, no `/Turning\|Turned/` | KEEP | age-suppression negative is the cited clause |

Outcome: keep as-is; add KNW-035 cites (coordinate with child 4).

#### `groups birthdays into today, upcoming, and already-celebrated` (L137) — cited CON-045[0]

KEEP (frozen-frame mock + section-scoped seeded names). Outcome: keep as-is.

#### `sorts upcoming birthdays soonest-first and sinks celebrated to the end` (L184) — cited CON-045[2]

KEEP — DOM-order over seeded cards + section-order comparison; ordering is the then-item. Outcome: keep as-is.

#### `the birthdays page date header follows the server accelerated frame` (L238) — cited CON-045[4]

KEEP (non-wall-clock frozen frame; rendered date is data). Outcome: keep as-is.

#### `shows the gift-planning section near year end` (L253) — cited CON-045[1]

KEEP. Outcome: keep as-is.

#### `hides the gift-planning section away from year end` (L274) — cited CON-045[1]

KEEP (settle-first negative). Outcome: keep as-is.

### 1.8 navigation.spec.ts

#### `imports nav item should have an icon element` (L9)

| Assertion | Verdict | Notes |
|---|---|---|
| link contains `svg` + `toHaveClass(/w-4.*h-4/)` | DELETE | pure DOM/CSS regression; icon sizing is judge territory |

Outcome: delete test.

#### `sync-pulse animation is defined in CSS` (L23)

| Assertion | Verdict | Notes |
|---|---|---|
| synthetic element + computed `animation` includes `sync-pulse` | DELETE | CSS keyframe existence check; unit tests already cover the sync-status class logic (per the file's own header comment) |

Outcome: delete test.

#### `imports link should be accessible from all main pages` (L47)

| Assertion | Verdict | Notes |
|---|---|---|
| Imports link visible on 5 routes | DELETE | strict subset of the DSH-002[0] loop below (which asserts all five links + hrefs on all five routes) |

Outcome: delete test.

#### `persistent nav links all five sections and marks the current one active` (L60) — cited DSH-002[0], DSH-002[1]

| Assertion | Verdict | Notes |
|---|---|---|
| all five links visible with correct hrefs, per route | KEEP | role + href |
| active link `toHaveClass(/border-blue-500/)`, inactive not | KEEP after rewrite | active-state → `aria-current="page"` on the nav Link (section 2: `navigation.tsx` currently exposes active state ONLY via class); then `toHaveAttribute('aria-current', 'page')` / absent |

Outcome: rewrite after aria addition; keep citations.

#### `navigation remains visible when scrolling` (L98) — cited DSH-002[2]

| Assertion | Verdict | Notes |
|---|---|---|
| nav visible after scroll; `boundingBox().y <= 5` | KEEP | behavioral stickiness proof (position after real scroll) — this is the resilient form of the sticky assertion |

Outcome: keep as-is.

#### `navigation has correct sticky classes` (L129) — cited DSH-002[2]

| Assertion | Verdict | Notes |
|---|---|---|
| `toHaveClass(/sticky/)`, `/top-0/`, `/z-50/` | DELETE | pure CSS re-statement of what the behavioral scroll test already proves for the same then-item |

Outcome: delete test (DSH-002[2] stays covered by the scroll test).

### 1.9 error-boundary.spec.ts

#### `backend test error endpoint returns 500` (L13)

| Assertion | Verdict | Notes |
|---|---|---|
| POST /test/trigger-error → 500 envelope | DELETE | self-test of a test-only backend endpoint; no behavior owns it and neither remaining test in the file uses the endpoint (both use route mocks). If the endpoint is now unused suite-wide, flag it for removal in a separate cleanup. |

Outcome: delete test.

#### `overdue loading shows placeholder skeletons, not an empty or caught-up state` (L32) — cited DSH-004[0], DSH-003[0]

| Assertion | Verdict | Notes |
|---|---|---|
| held-route gate; `.animate-pulse` first visible | KEEP after rewrite | loading state has no aria surface — add `role="status"` (+ accessible name) to the skeleton container (section 2), then `getByRole('status')`. Fallback: DSH visual-guard 1/2 (already explicitly commented). |
| no caught-up text, no Mark-as-Contacted buttons | KEEP | discriminating negatives |
| header CTA available while loading | KEEP | DSH-003[0] |
| release → caught-up renders | KEEP | proves loading was transitional |

Outcome: rewrite after aria addition; keep citations.

#### `overdue failure shows an error state with a reason, not empty or caught-up` (L77) — cited DSH-004[1], DSH-003[0]

| Assertion | Verdict | Notes |
|---|---|---|
| mocked 500 → `Error loading overdue contacts` heading | KEEP | error-state anchor |
| `xpath=following-sibling::p` reason visible + `/\S/` | KEEP after rewrite | structural xpath driver — add `role="alert"` to the error block (section 2), then `getByRole('alert')` and assert its text |
| no caught-up / no cards; header CTA present | KEEP | |

Outcome: rewrite after aria addition; keep citations; EXTEND to close orphan DSH-004[2] — assert the rendered reason contains the mocked message `Simulated overdue failure` and add `// spec: DSH-004[2]` (see section 4).

---

## 2. Aria surfaces the app must add

Targeted additions only (per the 2026-07-15 maintainer decision); each unblocks the named test rewrites.

| Component file | State currently class/DOM-only | Attribute to add | Tests that will assert it |
|---|---|---|---|
| `frontend/src/components/layout/navigation.tsx` (~L44-54) | active section link = `border-blue-500` | `aria-current="page"` on the active `Link` (conditional on `isActive`) | navigation.spec.ts `persistent nav links...` (DSH-002[1]) |
| `frontend/src/components/contacts/merge-contact-modal.tsx` FieldToggle (~L410-445) | selected source/target value = `bg-blue-600` | `aria-pressed={selected === 'target'}` / `aria-pressed={selected === 'source'}` on the two toggle buttons | contact-merge.spec.ts `should toggle field selection...` (CON-043[2]) |
| `frontend/src/components/contacts/merge-contact-modal.tsx` container (L168) | modal is an anonymous `.fixed.inset-0` div | `role="dialog"` + `aria-labelledby` pointing at the `Merge Contacts` heading (or `aria-label="Merge Contacts"`), `aria-modal="true"` | every kept contact-merge test (replaces the `.fixed.inset-0` locator) |
| `frontend/src/components/ui/contact-selector.tsx` (~L140, 199, 224) | dropdown options are `cursor-pointer select-none` divs | combobox/listbox semantics: `role="listbox"` on the panel, `role="option"` (+ `aria-selected`) on rows — verify first whether the underlying primitive already emits these; if so, tests switch to `getByRole('option')` with no app change | contact-merge.spec.ts source-selection steps in 6 kept tests |
| `frontend/src/components/contacts/tasks-section.tsx` Clock icon (L166) | follow-up pending indicator = `text-amber-400` class on an svg | `aria-label="Awaiting reply"` (or `title`) on the Clock icon — mirrors the CAD-029 indicator's existing `title="Awaiting reply"` | contact-tasks.spec.ts `lists follow-up tasks first...` (CAD-030[0]); frees CAD visual-guard budget |
| `frontend/src/components/contacts/tasks-section.tsx` card container (L53) | Tasks card located via `div.bg-white.shadow` | render as `<section aria-label="Tasks">` (or add `role="region"`) | the `tasksSection()` helper used by 6 contact-tasks tests |
| `frontend/src/app/dashboard/page.tsx` urgency dot (L89) | tier = `bg-yellow-500`/`bg-orange-500`/`bg-red-500` on a 3px dot | `aria-label`/`title` naming the tier (e.g. from `getUrgencyIndicator`, return label alongside class) | dashboard.spec.ts `each card shows urgency tier...` (CAD-026[1]); frees CAD visual-guard budget |
| `frontend/src/app/dashboard/page.tsx` loading skeletons (~L278) | loading = anonymous `animate-pulse` divs | `role="status"` + `aria-label="Loading overdue contacts"` on the skeleton container | error-boundary.spec.ts loading test (DSH-004[0]); frees DSH visual-guard budget |
| `frontend/src/app/dashboard/page.tsx` error block (~L296) | error reason reached via `xpath=following-sibling::p` | `role="alert"` on the error container | error-boundary.spec.ts failure test (DSH-004[1], DSH-004[2] backfill) |
| `frontend/src/components/ui/pagination.tsx` | — none needed — | `aria-current="page"` ALREADY present (L64); test rewrite only | contacts.spec.ts pagination test |
| `frontend/src/components/contacts/merge-contact-modal.tsx` quick-fill | `use this` located by text | verify it is a `<button>`; if not, make it one (accessible name `use this` or better) | contact-merge.spec.ts quick-fill test (CON-043[3]) |

## 3. Waivers to record

None. Both open orphans in this child's domains are deterministically provable (section 4). The three existing DSH-005 waivers stand; DSH-006/DSH-009 are `proposed` and exempt; CAD has no orphans.

## 4. Coverage gaps (backfill list)

### 4a. Scanner orphans

| Orphan | Resolution |
|---|---|
| CON-042[0] "a confirmation prompt warns the action cannot be undone" | (a) Extend the existing cited test `deletes a contact only after confirmation...` (contacts.spec.ts L471): the dismiss-path `page.once('dialog', ...)` handler captures `dialog.message()`; assert it contains `cannot be undone` (source copy at `src/app/contacts/[id]/page.tsx:213`), add `// spec: CON-042[0]`. 2-line change; the contact-tasks unlink test is the exact precedent. |
| DSH-004[2] "the shown failure reason faithfully reflects the actual failure" | (a) Extend the existing cited test `overdue failure shows an error state with a reason...` (error-boundary.spec.ts L77): the route mock already injects `message: 'Simulated overdue failure'`; apiClient plumbs the envelope's `error.message` into `ApiError.message` (`src/lib/api-client.ts:105-112`) and the dashboard renders `error.message` (`src/app/dashboard/page.tsx:299`) — so faithfulness IS deterministic here: `await expect(reason).toContainText('Simulated overdue failure')`, add `// spec: DSH-004[2]`. The in-file comment claiming this is judge-owned predates the route-mock rewrite and is wrong; update it. |

### 4b. Reverse gaps — kept tests with no behavior to cite (proposed mints, contacts domain)

These flows work today and are proved by kept tests, but no `surface: ui`, `status: current` behavior exists. Mint as `current` ux behaviors in `spec/contacts.yaml` (next free IDs from CON-053) in the same PR that adds the citations; grain below is a suggestion — the maintainer may consolidate B/E/F into one list-controls behavior.

| Mint | Suggested shape | Citing tests |
|---|---|---|
| CON-A: Log Interaction modal | given a contact detail page / when the user logs an interaction via the modal / then: [0] a direction is chosen from outbound, inbound, and mutual, defaulting to mutual; [1] an optional backdated date can be set; [2] the interaction is posted and the modal closes on success | contacts.spec.ts: backdated (L158), mutual-default (L284), outbound (L328), inbound (L378) |
| CON-B: list cadence filter | list filter select narrows to has-cadence / no-cadence via the server query | contacts.spec.ts cadence-filter (L576) |
| CON-C: create-from-form | submitting the new-contact form with a name creates the contact and lands on its detail page | contacts.spec.ts create @smoke (L691) |
| CON-D: detail method display | the detail page lists the contact's methods with normalized values and the primary marked | contacts.spec.ts methods display (L742, slimmed) — or decline the mint and delete the test |
| CON-E: column-header sorting | clicking a sortable column header drives the server sort param, toggling direction on repeat | contacts.spec.ts contact_by sort (L885), last_response_at sort (L925) |
| CON-F: list pagination controls | page-number and prev/next controls page the list, with the current page marked and boundary controls disabled | contacts.spec.ts pagination (L967) |
| CON-G: nav-bar prev/next buttons | the detail navigation bar's on-screen prev/next buttons traverse the list order (and a position indicator reports place) — deliberately a NEW behavior, not an edit to CON-040's then-list (index-shift hazard for its 4 citing tests) | contact-navigation.spec.ts button-nav (L247, rewritten) |
| CON-H: context carried across detail navigation | the sort/order/search context is carried in detail navigation URLs while moving prev/next — the current-status half of proposed CON-039 (which stays proposed for the tie-determinism claim) | contact-navigation.spec.ts context-preservation (L217) |

Cross-domain notes (owned by child 4, listed for coordination, no action here): NTS-007[2], NTS-008[2], KNW-035[0], KNW-035[1] receive citations from this child's files (section 5) — child 4 should count those covered rather than authoring duplicates.

### 4c. Surface-tag checks

No `surface` tag in CON/DSH/CAD looks wrong from this file set. CAD-023 carries `surface: ui` on an `api`-typed behavior — it is cited by overdue-contact-updates.spec.ts (out of this child's scope) and the tag is plausibly deliberate (the overdue payload is consumed only by the dashboard UI); flag for the parent to confirm, not a change request.

## 5. Citations to add to existing tests

| Test | Refs to add |
|---|---|
| contacts.spec.ts `should show expandable notes for long content` | `// spec: NTS-007[2]` |
| contacts.spec.ts `should not show expand button for short notes` | `// spec: NTS-007[2]` |
| contacts.spec.ts `should edit contact notes` | `// spec: NTS-008[2]` |
| contacts.spec.ts `deletes a contact only after confirmation...` | add `CON-042[0]` (with the dialog-message extension, section 4a) |
| contacts.spec.ts Log Interaction tests ×4 | `CON-A[...]` after mint |
| contacts.spec.ts `should filter contacts by cadence status` | add `CON-B` after mint (alongside existing DSH-007[0]) |
| contacts.spec.ts `should create a contact from the form` | `CON-C` after mint |
| contacts.spec.ts `should display contact with all methods...` (slimmed) | `CON-D` after mint |
| contacts.spec.ts sort tests ×2 (rewritten) | `CON-E` after mint |
| contacts.spec.ts pagination test (rewritten) | `CON-F` after mint |
| contact-navigation.spec.ts `should navigate via navigation bar buttons` (rewritten) | `CON-G` after mint |
| contact-navigation.spec.ts `should preserve URL context...` | `CON-H` after mint |
| contact-merge.spec.ts `should cancel name edit with Escape` | `// spec: CON-043[3]` |
| birthdays.spec.ts `shows placeholder-year birthdays without age...` | `// spec: KNW-035[0], KNW-035[1]` (detail-page assertions; coordinate with child 4) |
| error-boundary.spec.ts `overdue failure shows an error state...` | add `DSH-004[2]` (with the faithfulness extension, section 4a) |

## Implementation sequencing note

The aria additions (section 2) and the test rewrites that depend on them must land in the same PR per assertion site (delete-class-assert + add-aria-assert together, never a promissory split). The mints (4b) land with their citations. Pure deletions (19 tests) and the two orphan-closing extensions (4a) are independent and can land first — closing CON-042[0] and DSH-004[2] takes the scanner to 0 orphans across all three domains, making this child the natural place to flip `e2e_settled: true` for contacts, dashboard, and cadence-followup once the rewrites land.
