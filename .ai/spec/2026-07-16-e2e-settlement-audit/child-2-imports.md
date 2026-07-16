# Child 2 work-list: imports relax+cite (imports-matching / IMP)

Audit of the 9 `frontend/tests/e2e/imports-*.spec.ts` files (54 `test(` blocks) against `spec/imports-matching.yaml` (9 `surface: ui` behaviors: IMP-007, IMP-012, IMP-013, IMP-026..031) per the relaxation rubric in `.ai/spec/2026-07-15-remaining-e2e-migration-design.md`. Scanner baseline: 10 ui then-items covered (via three bare citations IMP-007/012/013 in imports-actions.spec.ts), 21 orphaned (all of IMP-026[0..3], IMP-027[0..4], IMP-028[0..2], IMP-029[0..2], IMP-030[0..2], IMP-031[0..2]).

Cross-cutting driver rot to fix wherever a test is kept (noted per-test as "driver rewrite"): candidate-card locator `page.locator('[class*="border-gray-200"]').filter({ hasText })` → role-scoped `page.locator('div.border', { has: page.getByRole('heading', { name }) })` or better a `getByRole('region')`/testid once the modal/card gets an aria surface; modal locator `.fixed.inset-0` → `getByRole('dialog')` (aria surface #2 below); heading locator `h3.text-lg` → `getByRole('heading')`; contact-selector option locator `[class*="cursor-pointer"]` → `getByRole('option')` or text within a role-scoped listbox.

Visual-guard count for this domain: 0 (no assertion here qualifies; nothing to budget).

## 1. Per-test triage

### imports-actions.spec.ts

#### `should display candidate cards with correct information` (line 40)

| Assertion | Verdict | Notes |
|---|---|---|
| `findCandidateByName(page, prefix-Test Import User)` (seeded candidate visible in queue) | KEEP | Cite `IMP-007[0]` (unmatched rows await review in the queue). Seeded-data-in-place. |
| `getByRole('button', { name: /Import/i }).first()` visible | KEEP (rewrite) | Global `.first()` is not this card's button — scope to the candidate card. Cite `IMP-027[1]` (user chooses import-as-new or link) at card level jointly with the Link assertion. |
| `getByRole('button', { name: /Link/i }).first()` visible | KEEP (rewrite) | Same scoping fix; jointly cites `IMP-027[1]`. |

Test outcome: rewrite (card-scope the two button assertions, add `// spec: IMP-007[0], IMP-027[1]`).

#### `should open link modal when clicking Link button` (line 52)

| Assertion | Verdict | Notes |
|---|---|---|
| `'Link to Existing'` mode-toggle button visible after opening modal | KEEP | Cite `IMP-027[1]`. Already role-based. |
| `getByText('Search for a contact...')` visible | KEEP (rewrite) | The select-a-contact affordance in link mode; rewrite to the selector's role (combobox/textbox placeholder) rather than bare copy. Supports `IMP-027[1]`. |
| Cancel click → mode toggle not visible | DELETE | Generic dialog-closes-on-cancel mechanics; no behavior owns it. |

Test outcome: rewrite (driver rewrite on card locator; drop cancel assertion; `// spec: IMP-027[1]`).

#### `should import candidate and show success notification` (line 102) — currently `// spec: IMP-012` (bare)

| Assertion | Verdict | Notes |
|---|---|---|
| import POST → 201 | KEEP | `IMP-012[3]` (with the two body assertions below). |
| response body carries `contact.id` | KEEP | `IMP-012[3]`. |
| response body carries `rematch_job_id` | KEEP | `IMP-012[3]`. |
| candidate card `not.toBeVisible` | KEEP | `IMP-031[0]` (item leaves its queue). Driver rewrite on card locator. |
| API re-read: `match_status === 'imported'` | KEEP | `IMP-012[2]`, `IMP-007[1]`. |
| API re-read: `crm_contact_id === createdContactId` | KEEP | `IMP-012[2]`. |
| GET contact: `full_name` matches | KEEP | `IMP-012[0]` (contact created through the normal path — E2E-observable slice; person-node/knowledge internals stay Go-owned). |
| GET contact: email method from external record present | KEEP | `IMP-012[1]` (else-branch: methods from the external record). |

Test outcome: keep, refine the bare citation to `// spec: IMP-012[0], IMP-012[1], IMP-012[2], IMP-012[3], IMP-007[1], IMP-031[0]`. Backfill (see section 4, IMP-031[2]): add `page.waitForResponse` for the frontend's `GET /api/v1/rematch/jobs/{id}` poll after import to prove the returned job was registered for polling → also cite `IMP-031[2]`.

#### `should ignore candidate and show notification` (line 191) — currently `// spec: IMP-007` (bare)

| Assertion | Verdict | Notes |
|---|---|---|
| ignore POST ok | KEEP | Flow gate for the state assertions. |
| ignore button `not.toBeVisible` | KEEP | `IMP-031[0]`. Driver rewrite on card locator. |
| API re-read: `match_status === 'ignored'` | KEEP | `IMP-007[3]`. |
| candidates list (limit=10000) excludes the id | KEEP | `IMP-007[3]` (sticky — never resurfaces in the queue). |

Test outcome: keep, refine bare citation to `// spec: IMP-007[3], IMP-031[0]`.

#### `should link candidate to existing contact` (line 274) — currently `// spec: IMP-013` (bare)

| Assertion | Verdict | Notes |
|---|---|---|
| link POST ok, body `match_status === 'imported'` | KEEP | `IMP-013[0]` (curation signal → imported; contact selection is curation). |
| body `crm_contact_id === targetContactId` | KEEP | `IMP-013[0]`. |
| candidate card `not.toBeVisible` | KEEP | `IMP-031[0]`. |
| API re-read: imported + linked id | KEEP | `IMP-013[0]`, `IMP-007[1]`. |

Test outcome: keep, refine bare citation to `// spec: IMP-013[0], IMP-007[1], IMP-031[0]`; driver rewrite (card locator, `[class*="cursor-pointer"]` option locator).

### imports-page.spec.ts

#### `should display page header and sync button` (line 18)

| Assertion | Verdict | Notes |
|---|---|---|
| heading 'Import Contacts' visible | DELETE | Static copy. |
| `Sync Contacts` button visible (`.first()`) | KEEP (rewrite) | Cite `IMP-026[2]`; also assert the `Sync Calendar` button (the then-item names contact AND calendar sources); drop `.first()` by scoping to the header toolbar region. |

Test outcome: rewrite into "offers manual sync triggers for contact and calendar sources" (`// spec: IMP-026[2]`), or merge into the trigger-sync test below.

#### `should show imports in navigation` (line 26)

| Assertion | Verdict | Notes |
|---|---|---|
| nav link 'Imports' visible | DELETE | No IMP behavior owns the nav entry; navigation chrome belongs to the navigation specs/judge. |

Test outcome: delete test.

#### `should display empty state when no Google Contacts candidates` (line 31)

| Assertion | Verdict | Notes |
|---|---|---|
| conditional: suggestions request carries `source=gcontacts` | KEEP (relocate) | The network-param proof of source filtering — move it into the filter test below (`IMP-026[0]`). |
| poll: empty-state copy OR candidate list visible | DELETE | The either-state poll makes it vacuous; TOCTOU-guarded conditional assertion. |
| 'All contacts from Google have been imported' copy | DELETE | Static empty-state copy; judge-owned. |

Test outcome: delete test (fold the `source=` request-param assertion into `should filter when clicking filter buttons`).

#### `should display source filter buttons` (line 81)

| Assertion | Verdict | Notes |
|---|---|---|
| `'Filter:'` label visible | DELETE | Static copy. |
| All Sources / Google Contacts / Calendar buttons visible | KEEP | `IMP-026[0]` (source filters exist). Role-based already. |
| `toHaveClass(/bg-blue-600/)` on All Sources (default selected) | KEEP (rewrite) | Rewrite to `toHaveAttribute('aria-pressed', 'true')` after aria surface #1 lands. `IMP-026[0]`. |

Test outcome: rewrite; merge with the test below into one "source filters" test. `// spec: IMP-026[0]`.

#### `should filter when clicking filter buttons` (line 93)

| Assertion | Verdict | Notes |
|---|---|---|
| `toHaveClass(/bg-blue-600/)` on Google Contacts after click | KEEP (rewrite) | → `aria-pressed` + assert the suggestions refetch carries `source=gcontacts` (network param). `IMP-026[0]`. |
| `not.toHaveClass(/bg-blue-600/)` on All Sources | KEEP (rewrite) | → `aria-pressed=false`. |
| `toHaveClass(/bg-blue-600/)` on Calendar after click | KEEP (rewrite) | → `aria-pressed` + `source=gcal_attendee` request param. |

Test outcome: rewrite (aria + network params). `// spec: IMP-026[0]`.

#### `should trigger sync when clicking sync button` (line 118)

| Assertion | Verdict | Notes |
|---|---|---|
| click sync → heading still visible ("page doesn't crash") | DELETE | Vacuous. |

Test outcome: rewrite: click each sync trigger and `waitForResponse` the `POST /api/v1/imports/sync` (assert the `source` param per button); `// spec: IMP-026[2]`. If the rewrite is not worth it, merge into the header-toolbar test and keep only the trigger-fires-request proof.

### imports-modal.spec.ts

#### `should close modal when pressing Escape key` (line 38)

| Assertion | Verdict | Notes |
|---|---|---|
| Escape → mode toggle not visible | DELETE | Generic dialog dismissal; no then-item owns it. |

Test outcome: delete test.

#### `should navigate with arrow keys` (line 61)

| Assertion | Verdict | Notes |
|---|---|---|
| heading changes on ArrowRight, returns on ArrowLeft, changes again | KEEP | `IMP-028[0]` (arrow keys move between candidates). Driver rewrite: `h3.text-lg` → `getByRole('heading')`, `.fixed.inset-0` → `getByRole('dialog')`. |

Test outcome: rewrite + extend: (a) also drive the position pager buttons (`aria-label` "Previous candidate"/"Next candidate" already exist) so the pager half of the then-item is proven; (b) add the inert-while-typing proof: focus the name input, dispatch ArrowRight, assert heading unchanged; (c) optionally assert the modal's candidate refetch carries `limit=1000` (the bound). `// spec: IMP-028[0]`.

#### `should close modal when clicking backdrop` (line 126)

| Assertion | Verdict | Notes |
|---|---|---|
| backdrop click → mode toggle not visible | DELETE | Generic dismissal mechanics. |

Test outcome: delete test.

#### `should display friendly source names with icons` (line 150)

| Assertion | Verdict | Notes |
|---|---|---|
| `span.text-gray-500` containing 'Google Contacts' visible | DELETE | Static label mapping + CSS-class locator; presentation the judge owns. |

Test outcome: delete test.

#### `should have transparent backdrop with blur` (line 176)

| Assertion | Verdict | Notes |
|---|---|---|
| `.fixed.inset-0.backdrop-blur-sm` visible | DELETE | Pure CSS-class assertion. |

Test outcome: delete test.

#### `should show cadence selector in import modal` (line 210)

| Assertion | Verdict | Notes |
|---|---|---|
| 'Contact Cadence' label visible | DELETE | Static copy. |
| 'How often you want to be reminded to reach out' visible | DELETE | Static copy. |
| select with 'No cadence' visible | DELETE | Subsumed by the data-asserting cadence tests below. |

Test outcome: delete test (coverage lives in `should import contact with selected cadence` + `should show cadence selector in link modal`).

#### `should show cadence selector in link modal` (line 252)

| Assertion | Verdict | Notes |
|---|---|---|
| 'Contact Cadence' visible | DELETE | Static copy. |
| select filtered by text 'Quarterly' visible | KEEP (rewrite) | The pre-fill proof: rewrite to `#contact-cadence` `toHaveValue('quarterly')`. Cite `IMP-027[4]` (link mode pre-fills cadence from the existing contact). |

Test outcome: rewrite. `// spec: IMP-027[4]`. Driver rewrite on card/option locators.

#### `should update cadence when linking contact` (line 306)

| Assertion | Verdict | Notes |
|---|---|---|
| `#contact-cadence` `toHaveValue('monthly')` (pre-fill) | KEEP | `IMP-027[4]`. |
| select weekly → `toHaveValue('weekly')` | KEEP | `IMP-027[4]` (a cadence can be chosen). |
| link POST awaited | KEEP | Flow gate; strengthen: assert the POST body carries `cadence: 'weekly'` (network param). |
| `getByText(/linked successfully/i)` toast | DELETE | Toast copy; the awaited response already proves success. |
| contact detail `getByTestId('contact-cadence')` contains 'weekly' | KEEP | Seeded-data-in-place on the dependent surface. `IMP-027[4]`. |

Test outcome: rewrite (drop toast, add request-body assertion, driver rewrites). `// spec: IMP-027[4]`.

#### `should default to no cadence in import mode` (line 401)

| Assertion | Verdict | Notes |
|---|---|---|
| cadence select `toHaveValue('')` | MOVE (merge) | Default-empty is not a then-item on its own; fold this single assertion into `should import contact with selected cadence` before selecting monthly. |

Test outcome: delete test after merging the assertion.

#### `should display clickable name in import modal` (line 451)

| Assertion | Verdict | Notes |
|---|---|---|
| modal `h3` visible | DELETE | Vacuous; subsumed by edit-mode tests. |

Test outcome: delete test.

#### `should enter edit mode when clicking name` (line 486)

| Assertion | Verdict | Notes |
|---|---|---|
| click heading → text input visible with candidate name value | KEEP | `IMP-027[2]` (name editable inline). Driver rewrite (`getByRole('textbox')`, dialog-scoped). |

Test outcome: keep (rewrite drivers); consider merging with the Enter-key test. `// spec: IMP-027[2]`.

#### `should confirm edit with Enter key` (line 530)

| Assertion | Verdict | Notes |
|---|---|---|
| Enter → heading shows new name | DELETE (merge) | Interaction detail fully subsumed by `should edit name and persist on import`, which proves the same path end-to-end. |

Test outcome: delete test (subsumed).

#### `should cancel edit with Escape key` (line 577)

| Assertion | Verdict | Notes |
|---|---|---|
| Escape → original name restored | DELETE | Generic edit-cancel mechanics; no then-item. |

Test outcome: delete test.

#### `should edit name and persist on import` (line 624)

| Assertion | Verdict | Notes |
|---|---|---|
| edited name shown in heading | KEEP | `IMP-027[2]`. |
| candidate name gone from list after import | KEEP | `IMP-031[0]`. |
| contacts search shows contact with edited name | KEEP (rewrite) | Prefer capturing the import POST (201 + `contact.id`) and GETting the contact by id — same proof without the search-page dependency. `IMP-012[0]` (name from the user's edit). |

Test outcome: keep, rewrite verification to API-read; driver rewrites. `// spec: IMP-027[2], IMP-012[0], IMP-031[0]`.

#### `should show star icons for method selection` (line 704)

| Assertion | Verdict | Notes |
|---|---|---|
| `button[title="Set as primary"]` visible | DELETE (merge) | Subsumed by the one-primary test. |

Test outcome: delete test.

#### `should select primary method by clicking star` (line 739)

| Assertion | Verdict | Notes |
|---|---|---|
| click star → `button[title="Primary contact method"]` visible | KEEP (merge+rewrite) | State via `title` swap → rewrite to `aria-pressed` (aria surface #3); merge into the one-primary test. `IMP-027[3]`. |

Test outcome: merge into the test below.

#### `should only allow one primary method at a time` (line 775)

| Assertion | Verdict | Notes |
|---|---|---|
| after first star click: primary count == 1 | KEEP (rewrite) | `IMP-027[3]` (at most one marked primary). Rewrite to `aria-pressed="true"` count once aria surface #3 lands. |
| after second star click: primary count still == 1 | KEEP (rewrite) | `IMP-027[3]`. |

Test outcome: keep as the single primary-method test (absorb line-739 test). `// spec: IMP-027[3]`.

#### `should show loading text during import action` (line 835)

| Assertion | Verdict | Notes |
|---|---|---|
| candidate card removed after import (only real assertion; loading text admittedly uncatchable) | DELETE | Redundant with imports-actions' import test (`IMP-031[0]` cited there). |

Test outcome: delete test.

#### `should import contact with selected cadence` (line 885)

| Assertion | Verdict | Notes |
|---|---|---|
| (merged in) cadence select `toHaveValue('')` default | KEEP | Setup context for `IMP-027[4]`. |
| select monthly, import, card gone | KEEP | `IMP-027[4]`, `IMP-031[0]`. Strengthen: assert the import POST body carries `cadence: 'monthly'`. |
| contact detail shows 'Contact cadence' + 'monthly' | KEEP (rewrite) | Rewrite to `getByTestId('contact-cadence')` `toContainText('monthly')` (or API GET on the created contact id) — drop the bare `getByText('monthly')`/label-copy assertions. `IMP-027[4]`. |

Test outcome: rewrite. `// spec: IMP-027[4], IMP-031[0]`.

### imports-features.spec.ts

#### `should show pagination when there are multiple pages` (line 24)

| Assertion | Verdict | Notes |
|---|---|---|
| Previous/Next buttons visible | KEEP | `IMP-026[0]` (candidates ... with pagination). |
| `getByText(/Page \d+ of \d+/)` | DELETE | Count-in-label. |

Test outcome: rewrite: with 21 seeded, click Next and assert the suggestions/candidates refetch carries the page-2 param (network param) and a this-worker candidate from the second page renders. `// spec: IMP-026[0]`.

#### `should show "Link (select)" when no suggested match` (line 49)

| Assertion | Verdict | Notes |
|---|---|---|
| card has button 'Link (select)' | KEEP | `IMP-029[1]` (without a suggestion, the link action asks the user to select). The accessible name IS the behavior here. Driver rewrite on card locator. |

Test outcome: keep. `// spec: IMP-029[1]`.

#### `should show suggested match with confidence percentage when present` (line 72)

| Assertion | Verdict | Notes |
|---|---|---|
| button `/Link to/` visible, contains suggested contact name | KEEP | `IMP-029[0]`. Seeded-data-in-place. |
| button contains '%' | KEEP | `IMP-029[0]` (confidence percentage). |

Test outcome: keep (driver rewrite on card locator). `// spec: IMP-029[0]`.

#### `should pre-select suggested contact in link modal` (line 117)

| Assertion | Verdict | Notes |
|---|---|---|
| 'Link Contact' enabled (proxy for pre-selection) | KEEP (strengthen) | `IMP-028[2]`. Add a direct proof: the ContactSelector shows the suggested contact's (seeded) name instead of the search placeholder. |
| Cancel → toggle not visible | DELETE | Generic dismissal. |

Test outcome: rewrite (strengthen + drop cancel). `// spec: IMP-028[2]`.

#### `should sort candidates by confidence score descending` (line 181)

| Assertion | Verdict | Notes |
|---|---|---|
| index-of-card ordering: high < medium < no-match | MOVE | Visual-ordering assertion re-checking IMP-010[0] (`surface: api` — "sorted by suggestion confidence descending ... unsuggested order by name"). Verify the Go API/handler suite covers global confidence ordering of `ListImportCandidates`/`BuildSortedCandidates`; author the Go test if missing, then delete this E2E test. |

Test outcome: delete test once the Go coverage is confirmed/authored.

#### `shows @username chip on Telegram candidate card` (line 269)

| Assertion | Verdict | Notes |
|---|---|---|
| heading shows display_name | KEEP | Seeded-data-in-place. |
| link with handle name visible + `href` = `https://t.me/<handle>` | KEEP | Data/attribute assertion. |

Test outcome: keep — but no IMP then-item owns the telegram identity chip. Flag: reveals a missing behavior; file a small `ux` behavior (telegram candidate cards surface the @username with a t.me deep link, falling back to the handle as the display name) and cite it from this + the next test. Until minted, the test stays uncited (settlement blocker for this file, so mint it in the same PR).

#### `falls back to @username when no name is set on Telegram candidate` (line 299)

| Assertion | Verdict | Notes |
|---|---|---|
| heading is the handle | KEEP | Data-in-place (fallback display name). Same missing-behavior flag as above. |
| chip link count == 0 when handle is the heading | KEEP | Same. |

Test outcome: keep; cite the newly minted telegram-display behavior.

#### `imports a handle-only Telegram candidate without requiring a name edit` (line 328)

| Assertion | Verdict | Notes |
|---|---|---|
| 'No contact methods available' `not.toBeVisible` | KEEP (rewrite) | Negative guard for a real bitten bug (PR #273); pair it with the positive row assertion so it is data-backed, and scope to the dialog. |
| `.space-y-2` region shows the handle as a method row | KEEP (rewrite) | CSS locator → dialog-scoped role/region. `IMP-012[1]` (methods from the external record — the row that will be imported). |
| success toast `${handle} imported successfully!` | KEEP (rewrite) | Replace toast-copy with `waitForResponse` on the import POST (201) + API-read of the created contact's `full_name === handle` and its telegram method — that is the actual claim (importable without a name edit, handle sent as name). `IMP-012[0]`, `IMP-012[1]`. |

Test outcome: rewrite. `// spec: IMP-012[0], IMP-012[1]`.

#### `shows @username method in Link to Existing modal` (line 375)

| Assertion | Verdict | Notes |
|---|---|---|
| 'No contact methods available' `not.toBeVisible` | KEEP (rewrite) | As above — pair with the positive assertion. |
| `.space-y-2` shows handle row ("Will be added"/"Same as CRM" grouping) | KEEP (rewrite) | `IMP-027[3]` (link mode distinguishes to-add from already-present). Rewrite locator; assert the group header the row sits under. |

Test outcome: rewrite. `// spec: IMP-027[3]`.

### imports-suggestions.spec.ts

#### `method-suggestion card appears at the top; Review confirms and clears it` (line 19)

| Assertion | Verdict | Notes |
|---|---|---|
| suggestion card visible (`prefix-Suggest Target — 1 new method`) | KEEP | `IMP-026[0]` (method suggestions group on the People tab). The "on top" position is not asserted — acceptable; positional ordering is judge-owned. |
| 'Adding to <contact>' header visible | KEEP | `IMP-030[0]` (target contact is fixed) — the name is seeded data in a template. |
| Import-mode button count == 0 | KEEP | `IMP-030[0]` (no import mode). |
| contact-search placeholder count == 0 | KEEP | `IMP-030[0]` (no contact selection). |
| Confirm → card gone | KEEP | `IMP-031[0]`. Strengthen: also `waitForResponse` the resolve POST and the invalidation-triggered suggestions refetch (GET after the action) → cites `IMP-031[1]` (queue surfaces refresh via invalidation, no reload); optionally API-read the contact to confirm the method landed (IMP-018 is api-surface, no cite needed). |

Test outcome: keep + strengthen. `// spec: IMP-026[0], IMP-030[0], IMP-031[0], IMP-031[1]`. Backfill within this test (section 4): deselect the sole method and assert Confirm is disabled → `IMP-030[2]`.

#### `Dismiss removes the card and it does not return after reload` (line 51)

| Assertion | Verdict | Notes |
|---|---|---|
| Dismiss → card gone | KEEP | `IMP-030[2]` (dismiss dismisses all pending), `IMP-031[0]`. |
| after reload card still gone (sticky) | KEEP | `IMP-030[2]` (stickily). |

Test outcome: keep. `// spec: IMP-030[2], IMP-031[0]`.

#### `link-only source hides Import on the card and in the modal` (line 74)

| Assertion | Verdict | Notes |
|---|---|---|
| card Import button count == 0 | KEEP | `IMP-029[2]`. |
| modal Import-as-New count == 0 and Link-to-Existing toggle count == 0 (locked) | KEEP | `IMP-027[1]` (link-only sources are locked to link). |

Test outcome: keep. `// spec: IMP-029[2], IMP-027[1]`.

#### `§4 residual: deselect-all link removes the candidate` (line 111)

| Assertion | Verdict | Notes |
|---|---|---|
| Link Contact enabled once contact selected | KEEP | Flow gate. |
| deselect-all loop via 'Deselect method' buttons | KEEP | `IMP-027[3]` (methods individually selectable). |
| link request body: `methods_curated === true`, `selected_methods == []` | KEEP | `IMP-013[0]` (frontend sends the curation signal; the status consequence is Go-owned by `TestLinkContact_MethodsCuratedDeselectAll_LandsImported`). |
| link response 200 | KEEP | Flow gate. |
| candidate card count == 0 after close | KEEP | `IMP-031[0]`. |

Test outcome: keep. `// spec: IMP-013[0], IMP-027[3], IMP-031[0]`. Strip the "§4 residual" plan-reference from the test name/comments per the metadata-reference rule.

### imports-interactions.spec.ts

#### `shows the amber badge and renders orphan cards on the Interactions tab` (line 45)

| Assertion | Verdict | Notes |
|---|---|---|
| Interactions tab `aria-selected=true` | KEEP | `IMP-026[1]` (tab exists/selected). Already aria-based. |
| both orphan titles visible | KEEP | `IMP-026[1]` (holds orphans). Seeded-data-in-place. |
| 'Open Anarlog' link `href === 'hyprnote://'` | KEEP | Attribute/data assertion (launch link on orphan cards). No then-item names the link — acceptable as supporting evidence under `IMP-026[1]`, or fold into the orphan-card claim. |
| (badge itself is NOT asserted despite the test name) | — | Backfill: assert the badge via its `aria-label` (`/\d+ needing attention/`, already in SubTabs) and, since this file is serial with a fresh singleton host, assert it is >= 2 for the two seeded orphans → completes `IMP-026[1]`'s badge clause. Discovery-exclusion half: seed an anarlog_title token in the same test and assert the badge count does not include it. |

Test outcome: rewrite (add badge assertions; rename accordingly). `// spec: IMP-026[1]`.

#### `accepts ?tab=needs-attention as a transitional alias for Interactions` (line 65)

| Assertion | Verdict | Notes |
|---|---|---|
| `aria-selected=true` on Interactions | KEEP | Route/aria assertion. |
| URL normalized to `tab=interactions` | KEEP | Route-state assertion. |

Test outcome: keep; cite `IMP-026[1]` (the tab surface). Note: the alias itself has no SSOT owner (transitional shim) — flag for eventual deletion when the alias is retired, or mint a then-item if it is meant to be durable.

#### `resolves the orphan via "Log as impromptu" (orphan_needs_review → linked_impromptu)` (line 78)

| Assertion | Verdict | Notes |
|---|---|---|
| resolve-link POST 200 | KEEP | Flow gate (the state transition itself is `surface: none` IMP-025, Go-owned). |
| resolved card's heading count == 0 | KEEP | `IMP-031[0]` (item leaves its queue). |
| sibling card still visible | KEEP | Scopes the action to the right item. |

Test outcome: keep; driver rewrite (`div.border-l-gray-300` → heading-scoped card locator without the color class). `// spec: IMP-031[0], IMP-026[1]`.

#### `scrolls to and highlights the ?session deep-linked card` (line 115)

| Assertion | Verdict | Notes |
|---|---|---|
| deep-linked card title visible | KEEP | Data-in-place. |
| `session=` param stripped from URL | KEEP | Route-state assertion (one-time-param rule). |
| (highlight/scroll not actually asserted) | — | Judge-owned visual concern; do not add a class assertion. |

Test outcome: keep — but no IMP then-item owns the `?session` deep link. Flag: reveals a missing behavior (deep link from a notification/daemon into the Interactions queue highlights the session's card and strips the one-time param); mint it and cite, or fold the URL-strip half under `IMP-026[1]` as supporting evidence and accept the test as the deep-link owner once minted.

### imports-correspondence.spec.ts

#### `evidence badge renders, Import is hidden, and link adds the method` (line 22)

| Assertion | Verdict | Notes |
|---|---|---|
| `Seen with <contact>` evidence text visible | KEEP | Metadata-driven seeded data (co-occurring contact name). No then-item owns the evidence badge — flag: missing behavior (gmail-correspondence candidates surface co-occurrence evidence: co-occurring contact + message count); mint and cite. |
| `4 messages` visible | KEEP | Seeded metadata count; same missing-behavior flag. |
| card Import button count == 0 | KEEP | `IMP-029[2]`. |
| link POST 200 | KEEP | Flow gate. Strengthen: API-read the target contact and assert the correspondence email is now a method ("link adds the method" is claimed in the test name but never asserted). |
| candidate card count == 0 after close | KEEP | `IMP-031[0]`. |

Test outcome: keep + strengthen (add the method API-read). `// spec: IMP-029[2], IMP-031[0]` + the newly minted evidence-badge behavior.

### imports-people.spec.ts

#### `shows the Anarlog source filter pill` (line 18)

| Assertion | Verdict | Notes |
|---|---|---|
| 'Anarlog' filter button visible | DELETE (merge) | Static presence; subsumed by the filtering test below which proves the pill works. |

Test outcome: delete test (merge into the test below).

#### `filters to anarlog_humans candidates and renders the Anarlog badge` (line 24)

| Assertion | Verdict | Notes |
|---|---|---|
| suggestions request carries `source=anarlog_humans` | KEEP | Network-param proof of source filtering. `IMP-026[0]`. |
| seeded candidate visible after filter | KEEP | Seeded-data-in-place. `IMP-026[0]`. |
| (the "Anarlog badge" in the name is not asserted) | — | Rename the test; the source badge is presentation (judge-owned). |

Test outcome: keep (rename; absorb the pill-visible assertion). `// spec: IMP-026[0]`.

### imports-name-candidates.spec.ts

#### `renders the name-candidate section with the grouped token and evidence count` (line 45)

| Assertion | Verdict | Notes |
|---|---|---|
| 'Names found in session titles' visible | KEEP (rewrite) | Use it as the region locator (role-scoped heading) rather than a copy assertion; the durable claim is the section appears when tokens exist. `IMP-026[3]`. |
| grouped token display name visible | KEEP | Seeded-data-in-place. `IMP-026[3]`. |
| `/Seen in 2 session titles/` visible | KEEP | Dynamic evidence count from the two seeded rows. `IMP-026[3]`. |

Test outcome: keep (rewrite the section locator to role-scoped). `// spec: IMP-026[3]`. The "only when tokens exist" negative half: the three resolution tests each end with the group gone; a full section-absent assertion after the last group resolves would complete it (cheap add to any one of them).

#### `imports the whole token group as a new contact` (line 84)

| Assertion | Verdict | Notes |
|---|---|---|
| dialog name-only branch ('No contact methods' note, Name value = token display) | KEEP | Dialog-scoped, role/label-based already. Group semantics (atomic resolve) are `surface: none` IMP-024, Go-owned. |
| scoped resolve POST ok | KEEP | Flow gate. |
| token display count == 0 after resolve | KEEP | `IMP-031[0]`. |

Test outcome: keep. `// spec: IMP-031[0]`.

#### `links the token group to an existing contact` (line 109)

| Assertion | Verdict | Notes |
|---|---|---|
| dialog opens, link-mode toggle, pick seeded contact | KEEP | Flow. |
| scoped resolve POST ok | KEEP | Flow gate. |
| token display count == 0 | KEEP | `IMP-031[0]`. |

Test outcome: keep. `// spec: IMP-031[0]`.

#### `ignores the token group via "Not a person"` (line 138)

| Assertion | Verdict | Notes |
|---|---|---|
| scoped resolve POST ok | KEEP | Flow gate. |
| token display count == 0 | KEEP | `IMP-031[0]`. |

Test outcome: keep. `// spec: IMP-031[0]`.

## 2. Aria surfaces the app must add

1. `frontend/src/app/imports/page.tsx` (~line 786, `SOURCE_FILTERS.map` buttons): source-filter pill selection is only observable via `bg-blue-600`. Add `aria-pressed={selected}`. Asserting tests: imports-page `should display source filter buttons` + `should filter when clicking filter buttons` (merged/rewritten).
2. `frontend/src/components/imports/suggestion-modal.tsx`: the candidate-resolution modal has no `role="dialog"`/`aria-modal`/accessible name. Add them so every modal test can scope via `getByRole('dialog')` instead of `.fixed.inset-0`. Asserting tests: all kept imports-modal tests, imports-actions link/import tests, imports-suggestions link-only test.
3. Primary-star toggle (rendered per method row via `suggestion-modal.tsx`/`method-selector.tsx`, currently `title="Set as primary"`/`title="Primary contact method"`): primary state is only a `title` swap. Add `aria-pressed` (keep the title as tooltip). Asserting test: imports-modal `should only allow one primary method at a time` (absorbing the star-click test).
4. `frontend/src/components/imports/method-selector.tsx` (line ~84): method selected state is `bg-blue-600` on the toggle; the accessible name swap ('Select method'/'Deselect method') exists but `aria-pressed` is the settled convention — add it. Asserting tests: imports-suggestions deselect-all test, the IMP-027[3] backfill test.

Already adequate (no change needed): SubTabs (`role="tab"` + `aria-selected` + badge `aria-label="N needing attention"`), unresolved-telegram toggle (`role="switch"` + `aria-checked`), pager buttons (`aria-label` Previous/Next candidate).

## 3. Waivers to record (spec/imports-matching.yaml)

| Behavior | then | Reason |
|---|---|---|
| IMP-007 | 2 | the matched (uncurated bare-link) state is unreachable from the UI — the modal always sends a curation signal; owned by the Go integration test TestLinkContact_Bare_LandsMatched |
| IMP-013 | 1 | same as IMP-007[2]: a bare link exists only at the API layer; owned by TestLinkContact_Bare_LandsMatched |
| IMP-027 | 0 | single-view/no-wizard is a structural-absence property with no deterministic observable; the IMP-027[1..4] citing tests all drive one dialog with no step transitions, and visual composition is judge-owned |
| IMP-031 | 1 | (fallback only — prefer the citing-assertion route in section 4; record this waiver only if the invalidation-refetch assertion proves flaky) scoped query invalidation is a frontend-unit mechanism asserted in use-imports.test.tsx; the user-visible outcome is proven under IMP-031[0] |

## 4. Coverage gaps (all 21 orphans)

| Orphan | Resolution |
|---|---|
| IMP-026[0] | (a) cite existing after rewrite: imports-page filter tests (merged), imports-features pagination (rewritten to network-param), imports-people filter test, imports-suggestions method-suggestion test. No new test needed. |
| IMP-026[1] | (a) cite imports-interactions badge/orphan-cards test after adding the badge assertions (aria-label count >= 2; discovery-exclusion via a seeded anarlog_title token not moving the count) + alias/resolve tests. Conflict-card rendering stays component/Go-owned (per the file's header rationale) — the then-item's "holds conflicts and orphans" is satisfied by the orphan half plus the conflict component tests; if the scanner is strict, extend the badge test to seed one conflict via TestAPI if/when a conflict seed route exists. |
| IMP-026[2] | (a) cite rewritten imports-page sync test (both Sync Contacts and Sync Calendar buttons + POST /imports/sync request param per source). |
| IMP-026[3] | (a) cite imports-name-candidates render test (rewritten section locator). |
| IMP-027[0] | (b) waiver — see section 3. |
| IMP-027[1] | (a) cite imports-actions card/link-modal tests (mode choice) + imports-suggestions link-only test (locked to link). |
| IMP-027[2] | (a) cite name-editing tests for "editable inline"; (a-new) backfill the two guards in one new test in imports-modal.spec.ts: seed 1 candidate → open modal → clear name to empty → attempt import → assert no POST fires and the modal stays (resolution blocked); then seed an unresolved telegram peer (telegram source, no name, no username) with `include_unresolved` toggled on → open modal → assert import is blocked until the name is edited, then import succeeds (2-4 lines each, waitForResponse + toHaveCount). |
| IMP-027[3] | (a) cite primary-method test (aria-pressed rewrite), deselect-all test, telegram link-modal test (to-add grouping); (a-new) backfill the two link-mode halves in one new imports-modal test: seed contact with email X + candidate with email X and phone Y → link mode → X's row is disabled/not re-selectable (aria/disabled), Y is selectable ("Will be added"); seed a same-type different-value email on the candidate → assert it is offered as an addition alongside the CRM value. |
| IMP-027[4] | (a) cite cadence link-modal pre-fill test + update-cadence test + import-with-cadence test. |
| IMP-028[0] | (a) cite arrow-keys test after extending it (pager buttons, inert-while-typing, optional limit=1000 request param). |
| IMP-028[1] | (a-new) author a new imports-modal test: seed exactly 2 candidates for this worker, filter to an isolating source, resolve the first via import → assert the dialog heading advances to the second candidate (not closed); resolve the second → assert the dialog closes (`getByRole('dialog')` count 0). Needs the queue scoped to this worker's source/filter to be deterministic under parallel workers. |
| IMP-028[2] | (a) cite imports-features pre-select test (strengthened). |
| IMP-029[0] | (a) cite imports-features confidence-percentage test. |
| IMP-029[1] | (a) cite imports-features Link-(select) test. |
| IMP-029[2] | (a) cite imports-suggestions link-only test + imports-correspondence test. |
| IMP-030[0] | (a) cite imports-suggestions method-suggestion test (fixed header, no selector, no import). |
| IMP-030[1] | (a-new) backfill: extend the method-suggestion resolver flow — seed a suggestion whose comparison set includes a method already on the contact (identical value) and assert that row's toggle is disabled (`toBeDisabled`). If the seed route cannot express an identical-method comparison, extend `method-suggestion-resolver.test.tsx` (which already covers the resolver's other rows) and cite from there only if the scanner accepts Vitest citations for ui items; otherwise record a waiver: "already-present rows are render-time derived state covered by the resolver component test; not seedable via the E2E TestAPI". Decide at implementation. |
| IMP-030[2] | (a) cite Dismiss-sticky test; (a-new) backfill the confirm-requires-one half: in the Review flow, deselect the sole method and assert Confirm `toBeDisabled` (one assertion inside the existing method-suggestion test). |
| IMP-031[0] | (a) cite the resolution tests across all files (actions import/ignore/link, suggestions confirm/dismiss/deselect-all, name-candidates x3, interactions resolve, modal persist-on-import, correspondence). |
| IMP-031[1] | (a-new) backfill: in the suggestions confirm test, after the resolve POST assert an invalidation-triggered refetch fires without reload (`waitForResponse` GET /api/v1/imports/suggestions following the action). If flaky in practice, fall back to the section-3 waiver. |
| IMP-031[2] | (a-new) backfill: in imports-actions' import test, after the 201 assert the frontend's poll `GET /api/v1/rematch/jobs/{rematch_job_id}` fires (`page.waitForResponse`) — proves the returned job was registered for polling. (The unit suite explicitly does not exercise the provider.) |

Surface-tag second looks (option c): none — all 9 ui behaviors are genuinely browser-observable. IMP-007[2]/IMP-013[1] are waived per-then-item rather than retagged because the rest of their then lists are UI-provable.

## 5. Citations to add to existing tests (already proving behavior, uncited)

| File / test | Refs to add |
|---|---|
| imports-actions `should display candidate cards with correct information` | `IMP-007[0], IMP-027[1]` |
| imports-actions `should open link modal when clicking Link button` | `IMP-027[1]` |
| imports-actions `should import candidate...` (refine bare `IMP-012`) | `IMP-012[0], IMP-012[1], IMP-012[2], IMP-012[3], IMP-007[1], IMP-031[0]` (+ `IMP-031[2]` after backfill) |
| imports-actions `should ignore candidate...` (refine bare `IMP-007`) | `IMP-007[3], IMP-031[0]` |
| imports-actions `should link candidate...` (refine bare `IMP-013`) | `IMP-013[0], IMP-007[1], IMP-031[0]` |
| imports-page merged source-filter test | `IMP-026[0]` |
| imports-page sync-trigger test (rewritten) | `IMP-026[2]` |
| imports-modal `should navigate with arrow keys` | `IMP-028[0]` |
| imports-modal `should show cadence selector in link modal` (rewritten) | `IMP-027[4]` |
| imports-modal `should update cadence when linking contact` | `IMP-027[4]` |
| imports-modal `should enter edit mode when clicking name` | `IMP-027[2]` |
| imports-modal `should edit name and persist on import` | `IMP-027[2], IMP-012[0], IMP-031[0]` |
| imports-modal `should only allow one primary method at a time` | `IMP-027[3]` |
| imports-modal `should import contact with selected cadence` | `IMP-027[4], IMP-031[0]` |
| imports-features pagination test (rewritten) | `IMP-026[0]` |
| imports-features `should show "Link (select)"...` | `IMP-029[1]` |
| imports-features `should show suggested match with confidence percentage...` | `IMP-029[0]` |
| imports-features `should pre-select suggested contact in link modal` | `IMP-028[2]` |
| imports-features telegram chip + fallback tests | newly minted telegram-display behavior (see triage) |
| imports-features `imports a handle-only Telegram candidate...` | `IMP-012[0], IMP-012[1]` |
| imports-features `shows @username method in Link to Existing modal` | `IMP-027[3]` |
| imports-suggestions `method-suggestion card appears at the top...` | `IMP-026[0], IMP-030[0], IMP-031[0]` (+ `IMP-030[2]`, `IMP-031[1]` after backfills) |
| imports-suggestions `Dismiss removes the card...` | `IMP-030[2], IMP-031[0]` |
| imports-suggestions `link-only source hides Import...` | `IMP-029[2], IMP-027[1]` |
| imports-suggestions deselect-all test | `IMP-013[0], IMP-027[3], IMP-031[0]` |
| imports-interactions badge/orphan-cards test | `IMP-026[1]` |
| imports-interactions alias test | `IMP-026[1]` |
| imports-interactions `resolves the orphan via "Log as impromptu"...` | `IMP-031[0], IMP-026[1]` |
| imports-correspondence evidence test | `IMP-029[2], IMP-031[0]` (+ minted evidence-badge behavior) |
| imports-people filter test | `IMP-026[0]` |
| imports-name-candidates render test | `IMP-026[3]` |
| imports-name-candidates import/link/ignore group tests | `IMP-031[0]` each |

## Tallies

- Test-level outcomes: 15 delete (page x3, modal x10, features x1 [MOVE], people x1), 39 keep (of which ~24 need driver/assertion rewrites), 2 new tests to author (IMP-027[2] guards, IMP-028[1] advance/close), plus small backfill assertions inside 5 existing tests.
- MOVE targets needing Go verification/authoring: 1 — global confidence ordering (IMP-010[0], `ListImportCandidates`/`BuildSortedCandidates` handler test). Bare-link semantics already Go-covered (`backend/tests/link_contact_curated_status_integration_test.go`); those Go tests should gain bare `// spec: IMP-007[2]` / `IMP-013[1]` markers alongside the section-3 waivers.
- New behaviors to mint in `spec/imports-matching.yaml` (tests exist, no owner): telegram candidate @username display/fallback; gmail-correspondence evidence badge; (optional) `?session` deep-link into the Interactions queue.
- Waivers: 3 firm + 1 conditional fallback (section 3).
