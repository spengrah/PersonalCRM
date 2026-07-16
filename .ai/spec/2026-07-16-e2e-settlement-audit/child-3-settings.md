# E2E settlement audit — child 3: settings (settings.spec.ts, settings-mac.spec.ts, telegram-settings.spec.ts)

Audit date: 2026-07-16. Files: `frontend/tests/e2e/settings.spec.ts` (6 tests), `frontend/tests/e2e/settings-mac.spec.ts` (7 tests), `frontend/tests/e2e/telegram-settings.spec.ts` (13 tests). Domains: settings (SET-019..028), mac-host (MAC-018), telegram (TGM-004/005/010), todoist (TDS-034/035). No `// spec:` citations exist in any of the three files today. All three files are mapped in `test-map.json` (`@area:settings`).

**Environment fact that shapes everything below:** the E2E backend sources the deployment `.env` (`scripts/start-backend.sh`), and the sandbox/CI env has no `GOOGLE_*`/`TODOIST_*` credentials and no `ENABLE_TELEGRAM_SYNC`. That is why settings.spec.ts is riddled with hedged either/or assertions (`isVisible().catch(() => false)`) — they are environment-conditional, i.e. non-deterministic by design. The fix is the sanctioned route-mock technique (precedent: contact-tasks.spec.ts mocks its own `/api/v1/contacts/:id/tasks*` routes): mock the app's own account/status endpoints per test to force each provider state deterministically, independent of deployment env.

**Visual-guard exceptions used: 0** (settings 0, mac-host 0, telegram 0, todoist 0).

## 1. Per-test triage

### settings.spec.ts

#### T1 `should display Todoist accounts section` (line 4)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| heading `Todoist` visible (line 9) | KEEP | SET-019[0] | Role-based already; section presence per provider is behavior-mandated. |
| hedged `hasConnectButton \|\| hasConfigMessage` (lines 13-20) | DELETE | — | Environment-conditional either/or can never fail meaningfully. Replace: route-mock `/api/v1/auth/todoist/accounts` → 404, assert the not-connected empty state (Connect button by role) AND the Configuration Required panel naming `TODOIST_CLIENT_ID`/`TODOIST_CLIENT_SECRET`/`TOKEN_ENCRYPTION_KEY` — cite SET-023[0], SET-023[1]. |

Test outcome: **rewrite** (deterministic via route-mock; cites SET-019[0], SET-023[0], SET-023[1]).

#### T2 `should display Google Accounts section with optional Gmail sync badge` (line 23)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| heading `Google Accounts` visible (line 30) | KEEP | SET-019[0] | Same section-presence rationale as T1. |
| conditional Gmail badge via `page.locator('div').filter({ hasText: /^Gmail/ })` + refresh button enabled (lines 35-46) | DELETE | replaced by SET-024[0] rewrite | Structure-based locator + if-present guard = dead branch in every env without a connected account. Replace with a route-mocked accounts response carrying a chosen scope subset; assert the matching sync affordances appear and the omitted-scope one does not (count 0 inside the aria-named section region). Needs the SyncBadge aria-label from section 2. |

Test outcome: **rewrite** into a deterministic scope-driven badge test citing SET-024[0].

#### T3 `should display optional Google Chat sync badge or reconnect hint` (line 49)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| conditional Chat badge / reconnect hint with nested if/else (lines 60-82) | DELETE | replaced by SET-024[1] rewrite | Both branches are env-conditional. Replace with two route-mocked cases: (a) account with all three chat scopes → Chat sync affordance present; (b) partial grant → `Chat — reconnect required` button present, Chat sync affordance absent. The pure scope-gate logic is already unit-tested (`google-accounts-section.test.tsx` covers `accountHasChatScopes` exhaustively) — the E2E proves only the surface branch. |

Test outcome: **rewrite** (two deterministic cases citing SET-024[1]).

#### T4 `should display settings page with export and import sections` (line 85)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| heading `Settings` visible (line 89) | DELETE | — | Static page heading, no behavior. |
| heading `Export Data` visible (line 92) | DELETE | — | Static copy; the export affordance is located by button role instead. |
| `Download Backup` button visible (line 93) | KEEP (rewrite) | SET-028[0] | Upgrade from presence to function: click → `page.waitForEvent('download')` + `waitForResponse` POST `/api/v1/export` status 200, download filename ends `.json`. |
| heading `Import Data` visible (line 96) | DELETE | — | Static copy. |
| file input visible + `accept=.json` attribute (lines 99-101) | KEEP (rewrite) | SET-028[1] | Attribute assertion is fine; upgrade: `setInputFiles` a tiny JSON fixture → click Validate → assert POST `/api/v1/import` request fired (handle the `alert()` via `page.on('dialog')`). |
| (missing) validation-mode note | KEEP (add) | SET-028[2] | Behavior-mandated communication ("import does not yet modify stored data"): assert a loose match (e.g. `/without making changes\|validation/i`) inside the import section region. Copy-asserting, but the then-item pins this exact communication — keep the match loose so a rewording within the same meaning survives. |

Test outcome: **rewrite** (cites SET-028[0], SET-028[1], SET-028[2]).

#### T5 `should have consistent form field styling` (line 104)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `toHaveClass(/rounded-md/)`, `/border/`, `/shadow-sm/` (lines 109-111) | DELETE | — | Pure CSS regression; judge-owned presentation. |

Test outcome: **delete test** (its only assertions are class checks).

#### T6 `should display system information section with version info` (line 114)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| heading `System Information`, text `Version` (lines 118-121) | DELETE | — | Static copy. |
| version fallback text via `p.text-gray-600:has-text("Version")` CSS locator → `Personal CRM (dev)` (lines 128-129) | DELETE | — | CSS-class locator + build-time env fallback string. The backend build-info stamp is already covered by `TestHealthEndpoint_StampedBuildInfo`; the frontend fallback branching is a candidate for a small Vitest component test if anyone wants it pinned (optional — no SSOT behavior mandates it). |
| no GitHub link rendered (line 132) | DELETE | — | Presentation detail of the same unpinned surface. |

Test outcome: **delete test**. No SSOT behavior covers the system-info panel; I recommend NOT minting one (dev-only fallback, judge can own the visual). If the maintainer disagrees, mint a settings ux behavior first, then re-add a data-asserting test.

### settings-mac.spec.ts

Serial mode (`describe.configure serial`, line 46) is justified by the mac_host partial unique index — keep it.

#### T1 `renders empty-state when no Mac hosts are paired` (line 48)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| heading `Mac Daemon` visible (line 52) | DELETE | — | Static heading; page load is proven by the list response wait below. |
| `Pair new Mac` button + back link visible (lines 56-57) | DELETE | — | Static structure; the pair affordance is exercised for real in T2. |
| text `No Mac hosts paired` (line 60) | KEEP (rewrite) | MAC-018[0] | Rewrite to data form: `waitForResponse` GET `/api/v1/host` and assert the response `data` array is empty, plus `getByTestId('mac-host-row')` has count 0. The empty list rendering is the zero-facet of "the list returns the live hosts". |

Test outcome: **rewrite** (cites MAC-018[0]).

#### T2 `opens pairing modal with a token when Pair new Mac is clicked` (line 63)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| initial GET `/host` waitForResponse hydration guard (lines 72-77) | KEEP | — (infra) | Uncited harness plumbing, keep. |
| POST `/api/v1/host/pairing-token` status 200 (lines 86-93) | KEEP | MAC-018[3] | Already compliant (network assert). |
| dialog role + aria-label `Pair new Mac` (lines 96-97) | KEEP | MAC-018[3] | Role-based. |
| `pairing-token-value` visible + not empty (lines 99-101) | KEEP | MAC-018[3] | Data assertion (minted token displayed). |
| Close dismisses modal (lines 104-105) | KEEP | — (generic) | Flow hygiene, no citation needed. |

Test outcome: **keep as-is**, add `// spec: MAC-018[3]`.

#### T3 `renders paired host with permissions and source-health badges` (line 108)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| admin-API verify of seeded host (lines 129-133) | KEEP | — (infra) | Seed sanity check. |
| row shows seeded hostname + host id (lines 146-147) | KEEP | MAC-018[0] | Seeded data located in a testid-scoped region. |
| permissions badges `Full Disk Access: granted` / `Contacts: denied` (lines 150-153) | KEEP | MAC-018[0] | Text is a rendering of the seeded permission booleans (`fda: true, contacts: false`) — data-derived, scoped to `permissions-badges`. |
| source-health shows `messages` + seeded cursor `abc-123` (lines 156-159) | KEEP | MAC-018[0] | Seeded data. |

Test outcome: **keep as-is**, add `// spec: MAC-018[0]`.

#### T4 `renders icloud_contacts contact count when backfill_complete (#327)` (line 164)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| waits for GET `/host` + GET `/host/:id/source-counts` (lines 200-212) | KEEP | — (infra) | Network asserts prove the counts flow is wired. |
| `3 contacts ✓` text (line 221) | KEEP (relax) | new MAC ux behavior (see section 4) | The count 3 derives from the 3 seeded external contacts — data. Relax the match to `/3 contacts/` (drop the `✓` glyph — presentation). |

Test outcome: **keep, relax the glyph**, cite the new backfill-count behavior once minted (interim: MAC-018[0] if the mint is deferred).

#### T5 `renders dash for icloud_contacts when backfill_complete is false (#327)` (line 227)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `rows.first().locator('td').nth(2)` toHaveText `—` (lines 260-262) | KEEP (rewrite) | new MAC ux behavior [1] | Positional `td.nth(2)` + glyph assertion is structure/presentation. Rewrite: give the cursor cell a `data-testid`/state attribute (section 2) and assert the cell is in its no-count state (and/or assert `/\d+ contacts/` has count 0 within `source-health`). |

Test outcome: **rewrite** the locator + assertion form.

#### T6 `opens rotate-key modal with templated CLI command when Rotate Key is clicked` (line 267)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| POST `/api/v1/host/pairing-token` status 200 (lines 294-301) | KEEP | MAC-018[3] | Rotate mints a fresh pairing token — the mint facet. |
| dialog role + aria-label (line 303) | KEEP | MAC-018[3] | Role-based. |
| command matches `^crm-mac install --re-pair --pair [A-Za-z0-9_-]{32,}$` (lines 309-312) | KEEP | MAC-018[3] | Data assertion: the minted token (format-checked) embedded in the operator command. The template prefix is stable operator contract, not styling. |
| Copy button → clipboard equals full command (lines 315-318) | KEEP | — (generic) | Functional data-equality; no dedicated behavior — acceptable uncited. |

Test outcome: **keep as-is**, add `// spec: MAC-018[3]`.

#### T7 `uninstall flow removes a paired host` (line 326)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| confirm dialog role `Uninstall Mac host` visible (lines 341-342) | KEEP | MAC-018[3] | Role-based confirm gate before revoke. |
| DELETE `/api/v1/host/:id` status 200 (lines 343-349) | KEEP | MAC-018[3] | The revoke facet, network-asserted. |
| `No Mac hosts paired` empty state (line 352) | KEEP (rewrite) | MAC-018[0] | Rewrite to the row-gone form: `mac-host-row` count 0 (or seeded hostname absent) after the list refetch — the removal reflected in the live list. |

Test outcome: **keep with minor rewrite**, cites MAC-018[3] + MAC-018[0].

### telegram-settings.spec.ts

All auth/chat tests route-mock the app's own `/api/v1/telegram/*` endpoints. That makes them valid frontend-flow tests, but it makes citing TGM-004/TGM-005/TGM-010 **circular** — those rows describe `AuthSessionManager`/`TelegramManager.Status` backend logic, which the mocks replace entirely. See section 4: the surface tags on those three behaviors look wrong, and replacement telegram `ux` behaviors should be minted for these tests to cite. Citations below marked "new TGM ux" bind to those mints.

#### T1 `shows Telegram section on settings page` (line 4)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| heading `Telegram` visible (line 9) | KEEP | SET-027[0] | Role-based section presence. |

Test outcome: **keep as-is**, add `// spec: SET-027[0]`.

#### T2 `shows not-configured or disconnected state` (line 14)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `configRequired.or(connectButton)` visible (lines 28-32) | DELETE | replaced by SET-027[1] rewrite | Hedged either/or. Rewrite: route-mock `/api/v1/telegram/auth/status` → 404 to force `not_configured` deterministically; assert the Configuration Required state naming `ENABLE_TELEGRAM_SYNC`, `TELEGRAM_API_ID`, `TELEGRAM_API_HASH` (behavior-mandated config list). |

Test outcome: **rewrite** (cites SET-027[1]).

#### T3 `auth flow: phone input shows on Connect click` (line 35)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `getByLabel('Phone Number')`, `Send Code`/`Cancel` buttons visible (lines 59-61) | KEEP | new TGM ux (connect flow) | Label/role-based, already compliant. |

Test outcome: **keep**, cite the minted connect-flow behavior.

#### T4 `auth flow: phone input → code input transition` (line 64)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `/code was sent to your Telegram app/i` (line 107) | KEEP | new TGM ux (connect flow: delivery channel reflected) | Data-driven: the message branch keys off the mocked `code_type: 'app'`. |
| `getByLabel('Verification Code')` visible (line 110) | KEEP | new TGM ux | Compliant. |
| (add) POST `/auth/start` request carries the entered phone | KEEP (add) | new TGM ux | Cheap `waitForRequest` upgrade: assert the network param, the strongest signal the UI wired the input to the API. |

Test outcome: **keep + small upgrade**, cite minted behavior.

#### T5 `auth flow: code → connected transition` (line 113)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `/Connected.*@testuser/` (line 172) | KEEP | new TGM ux (connect flow: code connects) | Username is mocked data reflected in the UI. |

Test outcome: **keep**, cite minted behavior.

#### T6 `auth flow: code → 2FA → connected transition` (line 175)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `/two-factor authentication/i` text (line 243) | DELETE | — | Static copy, redundant with the label assert on the next line. |
| `getByLabel('2FA Password')` visible (line 244) | KEEP | new TGM ux (connect flow: 2FA step) | Compliant. |
| `/Connected.*@testuser2fa/` (line 251) | KEEP | new TGM ux | Mocked data reflected. |

Test outcome: **keep, drop one copy assert**, cite minted behavior.

#### T7 `shows connected state with username` (line 254)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `/Connected.*@existinguser/` + phone text (lines 280-281) | KEEP | new TGM ux (connected status display) | Both values come from the mocked status — data. NOT TGM-010 (the mock replaces the resolution logic TGM-010 describes). |
| `Disconnect` button visible (line 282) | KEEP | new TGM ux | Role-based affordance. |

Test outcome: **keep**, cite minted behavior.

#### T8 `shows error on invalid code` (line 292)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `/Invalid verification code/i` (line 348) | KEEP | new TGM ux (verification error surfaced) | Data-driven: the string is the mocked error payload's message rendered to the user. Optionally also assert the code input is still present (flow retryable). |

Test outcome: **keep**, cite minted behavior.

#### T9 `group chat management: shows chat list` (line 351)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `page.locator('section', { has: heading })` scoping (lines 394-396) | KEEP (rewrite) | — | Structure-based scoping; switch to `getByRole('region', { name: 'Telegram' })` once the section gets an accessible name (section 2). |
| chat titles + member counts visible (lines 397-400) | KEEP | new TGM ux (chat tracking management) | Mocked data reflected in the list. |

Test outcome: **keep with locator rewrite**, cite minted behavior.

#### T10 `group chat management: empty state` (line 403)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `/No group chats discovered yet/i` (line 426) | KEEP (relax) | new TGM ux (chat tracking management: empty state) | Zero-chats data state; keep but loosen to a short stable anchor (e.g. `/No group chats/i`) and scope to the Telegram region. |

Test outcome: **keep with scoping**, cite minted behavior.

#### T11 `group chat management: backfill progress` (line 429)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `/Syncing messages.*15\/42/` (line 459) | KEEP | new TGM ux (backfill progress display) | The 15/42 figures are mocked data reflected in the UI. NOT TGM-010[0] (circular — the mock fabricates the enrichment the row describes). |

Test outcome: **keep**, cite minted behavior.

#### T12 `group chat management: toggle auto to ignored` (line 462)

| Assertion | Verdict | Citation / target | Notes |
|---|---|---|---|
| `page.locator('select').first()` (line 523) | KEEP (rewrite) | — | Structure-based; use `getByRole('combobox', { name: /Toggle Test Group/ })` after the aria-label from section 2. |
| `toHaveValue('auto')` → selectOption → `toHaveValue('ignored')` (lines 524-531) | KEEP | new TGM ux (tracking choice persists) | Element state assertion; the round-trip through the PATCH mock + refetch proves the UI persists and re-reads the choice. |
| (add) PATCH request body carries `status: 'ignored'` | KEEP (add) | new TGM ux | Network-param upgrade via `waitForRequest`. |

Test outcome: **rewrite locator + add request assert**, cite minted behavior.

#### T13 `group chat management: toggle auto to tracked` (line 534)

Same verdicts as T12 (lines 592-599), with `tracked`. Test outcome: **rewrite locator + add request assert**, cite minted behavior. Consider merging T12+T13 into one two-step test (auto→ignored→tracked) since they duplicate the harness wholesale.

## 2. Aria surfaces the app must add

| Component file | State / gap | Aria to add | Tests that will assert it |
|---|---|---|---|
| `frontend/src/components/settings/sync-badge.tsx` | SyncBadge refresh button is icon-only with no accessible name; tests currently reach it via `div.filter` structure locators | `aria-label={'Sync ' + label}` on the button (line 31) | rewritten settings.spec.ts T2/T3 (SET-024[0], SET-024[1]), new TDS-034 test |
| `frontend/src/components/settings/google-accounts-section.tsx`, `todoist-accounts-section.tsx`, `telegram-section.tsx` | provider `<section>` elements have no accessible name, so tests scope with `locator('section', { has: heading })` | `aria-label="Google Accounts"` / `"Todoist"` / `"Telegram"` (or `aria-labelledby` the heading) so `getByRole('region', { name })` works | nearly every rewritten settings/telegram test; telegram T9/T10 explicitly |
| `frontend/src/components/settings/telegram-section.tsx` (TelegramChatList, line 482) | per-chat status `<select>` has no accessible name; tests use `select.first()` | `aria-label={'Tracking for ' + (chat.chat_title \|\| 'Untitled')}` | telegram T12/T13 |
| `frontend/src/app/settings/mac/page.tsx` (source-health cursor cell, see `cursor-cell.ts`) | count-vs-dash backfill state is only distinguishable by rendered glyph in a positional `td` | `data-testid="cursor-cell"` plus a state hook (e.g. `data-state="count"\|"pending"`); testid not aria strictly, but it is the state surface these tests need | settings-mac T4/T5 |

Not needed: the telegram tracked/untracked dot exposes state only via `bg-green-500`/`bg-gray-300` classes, but no then-item requires asserting `effective_tracked` in the browser (TGM-011 is `surface: none`) — skip unless the minted chat-management behavior chooses to pin it, in which case add `aria-label` on the dot span.

## 3. Waivers to record

| Behavior | then | Reason (one line) |
|---|---|---|
| MAC-018 | 1 | The host detail read (including revoked hosts and unknown-id 404) is HTTP contract with no browser surface — the settings/mac page consumes only the list endpoint; owned by Go API tests (new test required, see section 4). |
| MAC-018 | 2 | The 404-vs-empty-count-map distinction is response-shape contract invisible in the browser (the UI renders only the happy-path counts); owned by Go API tests (`mac_host_source_counts_test.go`: UnknownHost_404, NoRowsReturnsEmpty). |

No SET waivers needed: all 24 SET then-items are deterministically E2E-provable via route-mocking. No TGM waivers if the section 4 retag+mint is accepted (waivers on TGM-004/005/010 would be the fallback if the maintainer rejects retagging — in that case waive every then-item of all three with reason "backend resolution logic; a route-mocked browser test is circular; owned by Go tests").

## 4. Coverage gaps (backfill list)

### settings (SET-019..028 — all 24 then-items orphaned)

All are (a) new-or-rewritten E2E tests in settings.spec.ts. Shared harness: route-mock `/api/v1/auth/google/accounts*`, `/api/v1/auth/todoist/accounts*`, `/api/v1/sync/states`, `/api/v1/todoist/*` as needed per test (own-backend mocking, sanctioned per contact-tasks precedent).

- **SET-019[0]** — covered by rewritten T1/T2 heading+section asserts (cite there; no new test needed).
- **SET-019[1]** — new test: mock google accounts list with one account (`account_id`, `created_at`); assert the card shows the account identity and its connected date inside the Google region. 2 lines of mock, data asserts.
- **SET-019[2]** — same new test: assert connect affordance (`Add Account` by role) and the per-account disconnect button exist.
- **SET-020[0]** — new test: mock the google auth-url endpoint to return a same-origin URL (e.g. `/settings?consent-stub=1`); click Connect; assert the auth-url request fired and `page.waitForURL` reaches the returned URL (whole-page navigation, `startGoogleOAuthFlow` sets `window.location.href`).
- **SET-021[0]** — new test: goto `/settings?auth=success&provider=google` with accounts mocked; assert success notification region appears and the accounts list refetches (`waitForResponse` on the accounts GET).
- **SET-021[1]** — new test: goto `/settings?auth=error&provider=google&message=exchange_failed`; assert the failure reason rendered (message derives from the URL param — data).
- **SET-021[2]** — same tests: `await expect(page).toHaveURL('/settings')` after the notification (params stripped via `history.replaceState`, fires ~500ms after mount).
- **SET-022[0]** — new test: mocked google account; `page.on('dialog')` captures the confirm; assert `dialog.message()` contains the account id and mentions revocation.
- **SET-022[1]** — same test, dismiss branch: dismiss the dialog, assert no revoke POST fired (track via a mock-side flag) and the account row remains.
- **SET-022[2]** — same test, accept branch: accept → assert POST `/accounts/:id/revoke` fired; flip the accounts mock to `[]` → assert empty state (removal reflected) + outcome notification.
- **SET-023[0]** — rewritten T1 (todoist) + a google twin: mock accounts route → 404; assert the not-connected empty state renders (no error stack).
- **SET-023[1]** — same tests: assert the Configuration Required panel names the provider's required config keys (behavior-mandated content, loose per-key match).
- **SET-024[0]** — rewritten T2: mock account with `gmail.readonly` + `calendar.readonly` only; assert Gmail and Calendar sync affordances present (aria-labeled buttons), Contacts affordance count 0.
- **SET-024[1]** — rewritten T3: full chat scopes → Chat sync affordance; partial → `Chat — reconnect required` button and no Chat sync affordance.
- **SET-025[0]** — new test: mock accounts + `/api/v1/sync/states` with an auth-error state newer than the account credential → Reconnect button visible for that account.
- **SET-025[1]** — same test, second case: account `updated_at` newer than the failing sync state → Reconnect button count 0.
- **SET-026[0]** — new test: mock todoist accounts + settings + projects + labels; assert both selects contain the mocked option names (data-populated pickers).
- **SET-026[1]** — same test: `selectOption` a project → assert the settings-update request body carries the chosen `project_id` (`waitForRequest`) + success notification.
- **SET-026[2]** — same test: with settings missing a label, assert the both-required note visible; after both chosen (mock updated), assert it is gone.
- **SET-027[0]** — telegram-settings T1 (cite only).
- **SET-027[1]** — rewritten telegram-settings T2.
- **SET-028[0..2]** — rewritten settings T4.

### mac-host (MAC-018[0..3])

- **MAC-018[0]** — covered: cite settings-mac T1 (rewritten empty facet) + T3 (seeded host rendered). No new test.
- **MAC-018[1]** — waiver (section 3) + **author new Go API test**: `backend/tests/api/` GET `/api/v1/host/:id` — returns a live host, still returns a revoked host, 404 for unknown id. Nothing covers `GetHostAdmin` today (grep of `backend/tests/api` shows no GET on the bare host-detail path).
- **MAC-018[2]** — waiver (section 3); Go coverage already exists uncited (section 5).
- **MAC-018[3]** — covered: cite settings-mac T2, T6, T7. No new test.
- **Missing behavior to mint (mac-host, `type: ux`, `surface: ui`, current):** "The source-health view reports a live contact count for a contacts source once backfill completes, and a neutral placeholder while backfill is in progress" (then: [0] count shown when `backfill_complete`, [1] placeholder when not). This is the #327 regression guard settings-mac T4/T5 already prove but cannot cite — today they'd have to free-ride on MAC-018[0].

### telegram (TGM-004[0..2], TGM-005[0..3], TGM-010[0..2])

**(c) Surface tags look wrong — all three.** TGM-004/005/010 are `type: business-logic` rows about `AuthSessionManager`/`TelegramManager.Status` internals (concurrent-start rejection, token expiry, session persistence with resolved user id, in-memory-vs-DB status resolution). None of that is provable in a browser test: the E2E suite necessarily route-mocks the telegram endpoints (MTProto cannot run in E2E), which replaces the exact logic these rows describe — any citation would be circular. Suggested retag: `surface: none` for all three (Go unit/integration tests over the managers with a stubbed MTProto client; the HTTP mapping is already separately owned by TGM-007 `surface: api`). Note the retag leaves those Go tests to be authored (none exist — no `_test.go` touches `StartAuth`/`VerifyCode`); that is a telegram-domain backfill item, not an E2E one.

**Mint replacement telegram `ux` behaviors** (`surface: ui`, current — they describe today's shipped UI) so the 11 existing flow tests have legal citation targets:

1. *Connect flow walks phone → code → optional password to connected* (then: connect reveals phone entry; a started flow advances to code entry reflecting the delivery channel; a valid code connects or advances to a password step; a valid password connects; connected shows the account username). Cited by T3, T4, T5, T6.
2. *The connected section reports identity and sync progress* (then: username and phone shown; backfill progress shown with completed/total while in progress; a disconnect affordance is offered). Cited by T7, T11.
3. *A rejected verification is surfaced and retryable* (then: the failure reason is shown; the flow stays on the same step for retry). Cited by T8.
4. *Group chats are listed with a per-chat tracking control* (then: discovered chats listed with member counts; an empty state before discovery; changing the tracking choice persists it and the list reflects it). Cited by T9, T10, T12, T13.

These mints live in `spec/telegram.yaml` (they are the UI half SET-027's notes explicitly defer to the telegram domain). Also sketch one **new E2E test** against mint 2's disconnect facet: the in-file comment (telegram-settings.spec.ts lines 285-290) claims route isolation of DELETE `/auth` vs GET `/auth/status` is impossible, but a single `**/api/v1/telegram/auth**` handler branching on `route.request().method()` (using `route.fallback()`, not `continue()`, for non-matching requests) isolates them fine — drive Disconnect, accept the confirm dialog, assert the DELETE fired and the section returns to the disconnected state.

### todoist (TDS-034[0..1], TDS-035[0..2])

- **TDS-034[0]** — new test in settings.spec.ts: mock todoist accounts + sync states; click the aria-labeled Tasks sync button → assert POST `/api/v1/sync/trigger` request with `source: 'todoist'` and the account id (network params) + success indication; second case mocks a 500 → failure indication.
- **TDS-034[1]** — same test: mocked account scopes include `data:read_write` → `Read & Write` permission badge visible in the account's Permissions & Sync region (data-derived from the mock).
- **TDS-035[0..2]** — **not this child's files.** TDS-035 renders on the contact page (`tasks-section.tsx`); its natural home is `contact-tasks.spec.ts` (already route-mocks `/contacts/:id/tasks*` and carries CAD-030/031/033 citations), which belongs to the cadence/contact-tasks child. That child should add TDS-035 citations (marker stripping → [0], deep-link href → [1], cadence-due absence on the contact page → [2]). Flagging here rather than claiming.

## 5. Citations to add to existing tests (no rewrite required)

| Test | Refs to add |
|---|---|
| telegram-settings.spec.ts T1 `shows Telegram section on settings page` | `// spec: SET-027[0]` |
| settings-mac.spec.ts T2 `opens pairing modal with a token…` | `// spec: MAC-018[3]` |
| settings-mac.spec.ts T3 `renders paired host with permissions and source-health badges` | `// spec: MAC-018[0]` |
| settings-mac.spec.ts T6 `opens rotate-key modal with templated CLI command…` | `// spec: MAC-018[3]` |
| settings-mac.spec.ts T7 `uninstall flow removes a paired host` | `// spec: MAC-018[3]` (plus MAC-018[0] after its small rewrite) |

Supporting Go citations (validated for deadness only, do not count toward ui coverage — add for traceability alongside the section 3 waivers): `backend/tests/api/mac_host_source_counts_test.go` `TestMacHost_GetSourceCounts_UnknownHost_404` and `TestMacHost_GetSourceCounts_NoRowsReturnsEmpty` → `// spec: MAC-018[2]`; the new host-detail Go test (section 4) → `// spec: MAC-018[1]`.

## Execution notes for the implementing child

- Land the section 2 aria additions in the same PR as the tests that assert them (targeted a11y is in scope per the design doc's resolved decision).
- The TGM retag + mints and the MAC mint are `spec/*.yaml` edits — do them first (with `make spec-lint`), since the citation indexes in the rewritten tests depend on the minted then-lists.
- settings.spec.ts currently uses `networkidle` + `reload` (lines 5-6 etc.) — replace with `domcontentloaded` + response waits per the repo gotcha while rewriting.
- After the work lands, `make spec-coverage` should show settings/mac-host/telegram/todoist ui orphans at 0 (modulo the two MAC waivers, reported loudly) — the `e2e_settled: true` flip is child 7's call, not this child's.
