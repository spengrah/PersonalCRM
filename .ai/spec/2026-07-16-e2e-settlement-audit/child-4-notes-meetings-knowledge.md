# Child 4 work-list: notes-meetings-knowledge (meetings.spec.ts + notepad + year-less birthday)

Audit per the 2026-07-15 remaining-e2e-migration design. Assigned file: `frontend/tests/e2e/meetings.spec.ts`. Domains: calendar (CAL-024..030), notes-meetings (NTS-007, NTS-008), knowledge (KNW-034, KNW-035). CAL-019 is already cited by `rematch-on-add-email.spec.ts` (another child's file) — no action here.

Summary of the shape: every assertion in meetings.spec.ts is a KEEP (the file is already role-based and data-asserting; it just needs citations, minor scoping rewrites, and a few extensions). The NTS orphans are partially covered uncited by `contacts.spec.ts` (child 5's file — citations noted in section 5), and KNW-035 is fully covered uncited by `birthdays.spec.ts` (child 6's file — section 5). The genuine gaps are: meeting-card content facets (CAL-026), past ordering (CAL-025[3]), reveal-reset (CAL-028[2]), the settings-side calendar behaviors (CAL-029/030 — new route-mocked tests), notepad line breaks + save/clear semantics (NTS-007[1], NTS-008[0..1]), and the contact-detail knowledge rows (KNW-034). Zero waivers proposed; zero visual-guard exceptions used (budget 2/domain, count 0).

## 1. Per-test triage table

### meetings.spec.ts — `should display meetings section with upcoming and past events` (line 35)

| Assertion (short quote) | Verdict | Citation / notes |
|---|---|---|
| `getByRole('heading', { name: /Meetings/i })` visible after seeding 4 events (l.49) | KEEP | `CAL-024[0]`. Compliant (role-based, presence keyed on seeded data). |
| `getByRole('button', { name: /All \(4\)/i })` visible (l.52) | KEEP | `CAL-025[0]`. Compliant — the count in the accessible name is the live count the then-item demands, derived from seeded data; this is NOT the rubric's static count-in-label DELETE case. |
| `Upcoming \(2\)` / `Past \(2\)` buttons visible (l.53-54) | KEEP | `CAL-025[0]`. Same reasoning. |
| default view: upcoming titles visible, past titles `not.toBeVisible()` (l.57-60) | KEEP | `CAL-025[1]`. Rewrite: scope the `getByText` seeded-title lookups to the Meetings region (`page.getByRole('heading', { name: /Meetings/i }).locator('..')` ancestor or a `region` scope) for strict-mode/parallel safety; once aria lands (section 2), also assert `aria-pressed="true"` on Upcoming pre-click. |
| click All → all four seeded titles visible (l.63-67) | KEEP | `CAL-025[0]`. Compliant. |

Test-level outcome: keep; add citations `// spec: CAL-024[0]`, `// spec: CAL-025[0], CAL-025[1]` at the assertion blocks; scope title lookups to the Meetings region.

### meetings.spec.ts — `should filter by upcoming events` (line 70)

| Assertion | Verdict | Citation / notes |
|---|---|---|
| click `Upcoming \(1\)` → upcoming title visible, past title not (l.81-85) | KEEP | `CAL-025[0]`. Largely redundant with test 1's default-view assertions but proves the explicit upcoming-filter activation. |

Test-level outcome: merge into the past-filter test below as a single `filters between upcoming and past` test (one seed, click Upcoming then Past), citing `CAL-025[0]`; keeping it standalone with the citation is also acceptable.

### meetings.spec.ts — `should filter by past events` (line 88)

| Assertion | Verdict | Citation / notes |
|---|---|---|
| click `Past \(1\)` → past title visible, upcoming title not (l.99-103) | KEEP | `CAL-025[0], CAL-025[2]` — the seeded `is_past` event's end time precedes the app's accelerated now, and that classification is exactly what routes it into the Past bucket (component classifies client-side via `useAcceleratedTime`). |
| `getByText('Past', { exact: true })` badge visible (l.106) | KEEP | `CAL-026[3]` (the past marker). Rewrite: scope to the meeting card containing the seeded past title (unscoped `getByText('Past')` risks strict-mode collisions with other page text). The "visually de-emphasized" facet of CAL-026[3] (opacity-60) is presentation the agentic judge owns — the marker assertion is the deterministic proof of the item; no waiver needed. |

Test-level outcome: keep (or merged per above); add citations; scope the badge assertion to the card.

### meetings.spec.ts — `should display html_link as clickable external link` (line 109)

| Assertion | Verdict | Citation / notes |
|---|---|---|
| `getByRole('link', { name: ... seeded title })` visible (l.127) | KEEP | `CAL-027[0]`. Compliant. |
| `toHaveAttribute('target', '_blank')` (l.130) | KEEP | `CAL-027[0]`. Attribute assertion — compliant. |
| `toHaveAttribute('href', <seeded link>)` (l.131) | KEEP | `CAL-027[0]`. Seeded data. |

Test-level outcome: keep as-is + citation. Extend in the same test for `CAL-027[1]`: seed a second event without `html_link`, assert its title text is visible in the card but `getByRole('link', { name: <title> })` resolves to zero elements (plain text, not a link).

### meetings.spec.ts — `should not show meetings section when no events exist` (line 137)

| Assertion | Verdict | Citation / notes |
|---|---|---|
| Meetings heading `not.toBeVisible()` with no events seeded (l.143) | KEEP | `CAL-024[1]` (the no-events half). Compliant. |

Test-level outcome: keep + citation. Extend for the fail-to-load half of the same then-item: `page.route('**/api/v1/contacts/*/events**', fulfill 500)`, reload, assert the heading stays absent (the component returns null on error) — sanctioned route-mock technique.

### meetings.spec.ts — `should show load more button when many events exist` (line 146)

| Assertion | Verdict | Citation / notes |
|---|---|---|
| `getByRole('button', { name: /Load more/i })` visible with 15 seeded (l.160) | KEEP | `CAL-028[0]`. Needs rewriting: assert the accessible name reports the remainder — `{ name: /Load more \(5 remaining\)/i }` (data-derived count, demanded by the then-item) — and assert exactly 10 seeded-title cards render initially (count within the Meetings region). |
| click → Load more `not.toBeVisible()` (l.167) | KEEP | `CAL-028[1]`. With 15 seeded and a 10-per-page reveal, one activation exhausts the list and the control disappears. Strengthen: assert all 15 seeded titles are visible post-click (seeded-data-in-place). |

Test-level outcome: rewrite per above + citations `CAL-028[0]`, `CAL-028[1]`. Extend (here or as a sibling test) for `CAL-028[2]` — see section 4.

No DELETE, MOVE, or DROP verdicts in this file: the suite's smallest spec is already in the settled style.

## 2. Aria surfaces the app must add

| Component file | State | Aria attribute | Tests that will assert it |
|---|---|---|---|
| `frontend/src/components/contacts/meetings.tsx` (filter buttons, l.191-206) | Active filter is only observable via the `bg-gray-900` class | `aria-pressed={filter === key}` on each filter button | meetings.spec.ts test 1 (CAL-025[1]: Upcoming pressed by default), the merged filter test (pressed state follows clicks) |
| `frontend/src/components/settings/sync-badge.tsx` (refresh button, l.32-38) | Not a state gap but a naming gap: the button is icon-only with no accessible name, so no role-based driver can reach it | `aria-label` = `Sync ${label} now` (per-badge, e.g. "Sync Calendar now") | new CAL-029 tests (section 4, items 6) |

The contact-detail notes expand control (`contacts/[id]/page.tsx` l.528) already carries text content ("Show more"/"Show less") — no aria change needed.

## 3. Waivers to record

None. Every orphaned then-item in these domains is deterministically provable (facets that are not — the opacity de-emphasis in CAL-026[3], the transient-loading half of CAL-030[1] — sit inside items whose other facets carry the deterministic proof, so the items are cited without waivers and the visual/transient facets fall to the judge).

## 4. Coverage gaps (backfill list)

Orphans NOT resolved by citing existing tests (those resolved by citation alone are in section 5). All meetings.spec.ts extensions reuse the existing `seedCalendarEvents` helper; note the backend seed input (`backend/internal/api/handlers/test.go` `SeedCalendarEventInput`) validates `title` as `required,min=1`, so the untitled-fallback facet needs a route-mock (below) or a seed-validator relaxation.

1. **CAL-025[3]** (past most-recent-first) — extend meetings.spec.ts: seed 3 past events with `days_ago: 1/5/10`, open Past filter, read the card titles in DOM order within the Meetings region (`allInnerTexts()` on the card title locator) and assert the seeded-title sequence is days_ago 1, 5, 10. Deterministic data ordering (the birthdays.spec.ts CON-045[2] precedent), not visual-ordering DELETE.
2. **CAL-026[0]** (title/fallback/date/time-range) — new meetings.spec.ts test: seed 1 event with known `days_ahead`; compute expected date and start-end strings from `GET /api/v1/system/time` the way birthdays.spec.ts computes expected dates (seeded events start 2 PM local, 1h duration); assert the card (scoped to seeded title) contains them. For the untitled fallback: `page.route` the `**/api/v1/contacts/*/events**` response to include one event with `title: ''` and assert the fallback label renders in the card (route-mock is the sanctioned technique; alternatively relax the seed validator to allow empty titles — flag for the implementer to choose).
3. **CAL-026[1]** (location when present) — same new test: seed one event with `location`, one without; assert the location value visible in the first card and absent from the second.
4. **CAL-026[2]** (attendee count only when >1) — same new test: seed one event with `attendee_emails: [a, b]` and one with none; assert "2 attendees" appears only in the multi-attendee card (count is data derived from the CAL-023 `attendee_count` projection).
5. **CAL-028[2]** (filter switch resets reveal) — extend the load-more test: after exhausting Upcoming (15 seeded), click All; assert the Load more control reappears reporting `(5 remaining)` and only 10 cards show — the reveal reset back to the initial page.
6. **CAL-029[0] + CAL-029[1]** (settings calendar sync control) — new test, proposed new file `frontend/tests/e2e/settings-calendar.spec.ts` (avoids colliding with child 3's settings.spec.ts work; needs a `test-map.json` entry → `@area:settings`; coordinate with child 3 if they'd rather host it). Route-mock the provider-dependent state: `GET /api/v1/auth/google/accounts` → one connected account with the calendar scope; `GET /api/v1/sync/status` → a gcal SyncState with a `last_successful_sync_at`; `POST /api/v1/sync/gcal/trigger` → 202. Drive: settings page, locate the Calendar sync badge, assert the state text reflects the mocked last-sync; click the (newly aria-labeled) sync button and `waitForRequest` on the trigger POST asserting the account id param — that proves [0]. For [1]: assert the started notification appears, then flip the mocked `/sync/status` to `status: 'syncing'` and assert the badge reports it (and optionally an `error` state variant). Depends on the section 2 SyncBadge aria-label.
7. **CAL-030[0]** (staleness banner names Google Calendar) — settings-calendar.spec.ts: `page.route('**/api/v1/sync/staleness', ...)` → one breach `{ source: 'gcal', breach_type: 'pull_staleness', details: ... }`; goto /settings; assert the `role="status"` banner is visible and contains "Google Calendar" (label derived from the mocked source datum).
8. **CAL-030[1]** (banner quiet when nothing stalled) — sibling test: mock staleness → `[]`; assert the banner (`role="status"` / `sync-staleness-banner` testid) is not present after the accounts section has rendered (settled-state absence, not a mid-load race; the transient loading facet is inherent in the same assertion since the banner renders null while loading).
9. **NTS-007[0]** (notepad appears only with content) — new file `frontend/tests/e2e/notepad.spec.ts` (`test-map.json` → `@area:contacts`): seed a contact, goto detail, assert no Notes row renders (scoped to the details `<dl>` region); then `seedContactNote`, reload, assert the row with the seeded body is visible. One test proves both halves. (The presence half is also citable in contacts.spec.ts — section 5.)
10. **NTS-007[1]** (line breaks preserved) — notepad.spec.ts: seed a note whose body has two paragraphs separated by a blank line; locate the note body and assert `innerText()` still contains the `\n\n` separator (the `whitespace-pre-wrap` rendering contract, asserted as data not class).
11. **NTS-008[0]** (notepad saved as its own operation, both done before edit closes) — notepad.spec.ts: open Edit on the detail page, change Full Name and Notes, submit with `Promise.all` of `waitForResponse` on `PUT **/api/v1/contacts/<id>` and `PUT **/api/v1/contacts/<id>/notes` (both 2xx) plus the click; then assert edit mode closed (Edit button visible) and both new values render. Network-level proof of the two parallel operations (`handleUpdateContact` fires both via `Promise.all`).
12. **NTS-008[1]** (clearing removes the note) — notepad.spec.ts: seed note, edit, clear the Notes field, submit; assert the `PUT .../notes` response status is 204 (the delete contract) and the Notes row is gone after edit mode closes; optionally re-`GET` the notepad via `request` and assert 204.
13. **KNW-034[0]** (location row only when known) — new file `frontend/tests/e2e/contact-knowledge.spec.ts` (`test-map.json` → `@area:contacts`): create contact A via `POST /api/v1/contacts` with `location`, contact B without; on A's detail assert the Location row's value (seeded label) is visible; on B assert no Location row renders in the details region.
14. **KNW-034[1]** (birthday row only when known) — same file/test pattern: create contact with a real-year `birthday` (direct POST, as birthdays.spec.ts's `createContactWithBirthday` does — `seedContacts` has no birthday field), assert the Birthday row shows the human-readable `formatBirthday` output for the seeded date; contact without birthday shows no row. (The known-half also gets partial proof from birthdays.spec.ts l.120 — but that test's detail assertion is placeholder-year-specific, so the labeled-row + absent-row proof belongs here.)

No surface-tag corrections proposed: all 29 orphans in these domains are genuinely browser-observable.

## 5. Citations to add to existing tests

In my assigned file (this child applies them):

- meetings.spec.ts per section 1: `CAL-024[0]`, `CAL-024[1]`, `CAL-025[0]`, `CAL-025[1]`, `CAL-025[2]`, `CAL-026[3]`, `CAL-027[0]`, `CAL-028[0]`, `CAL-028[1]`.

In other children's files (note for the owning child; do not edit from this child):

- **contacts.spec.ts** (child 5): `should show expandable notes for long content` (l.86) → `// spec: NTS-007[0], NTS-007[2]` (content renders; overflow control appears and toggles). `should not show expand button for short notes` (l.132) → `// spec: NTS-007[0], NTS-007[2]` (control appears ONLY on overflow — the absent half). `should edit contact notes` (l.705) → `// spec: NTS-008[2]` (displayed notepad reflects new content after save); optionally also `NTS-008[0]` if child 5 adds the dual `waitForResponse` rewrite, though item [0] is independently covered by this child's notepad.spec.ts sketch (item 11).
- **birthdays.spec.ts** (child 6): `shows placeholder-year birthdays without age and keeps today at the top` (l.84, currently cited `CON-045[3]` only) → add `// spec: KNW-035[0], KNW-035[1]` — it already asserts the month-day-only rendering with the placeholder year suppressed across the list, detail, and birthdays surfaces, and the no-age fact (`not.toContainText(/Turning|Turned/)`). This fully resolves both KNW-035 orphans by citation alone.
- **rematch-on-add-email.spec.ts** (another child): already cites `CAL-019` — no action, listed for completeness.
