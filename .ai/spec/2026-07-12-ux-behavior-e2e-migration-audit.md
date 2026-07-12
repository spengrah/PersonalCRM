# UX behavior → E2E migration audit

Repo `/Users/spencer/Workspaces/PersonalCRM`, branch `develop`. Read-only audit of every `type: ux` behavior in `spec/*.yaml`, each `then`-item bucketed as ALREADY-COVERED (an existing Playwright spec asserts it), NEEDS-NEW-E2E (deterministic, no E2E asserts it today), or KEEP-FOR-JUDGE (not deterministically assertable).

## Summary counts

**56 `type: ux` behaviors across 10 domains; 153 then-items.** (`ingest` and `telegram` have zero `ux` behaviors.)

| Bucket | Count | Share |
|---|---|---|
| ALREADY-COVERED | 64 | 42% |
| NEEDS-NEW-E2E | 86 | 56% |
| KEEP-FOR-JUDGE | 3 | 2% |

By domain:

| Domain | Behaviors | Then-items | ALREADY-COVERED | NEEDS-NEW-E2E | KEEP-FOR-JUDGE |
|---|---|---|---|---|---|
| cadence-followup | 7 | 22 | 6 | 16 | 0 |
| calendar | 7 | 19 | 9 | 10 | 0 |
| contacts | 8 | 24 | 13 | 10 | 1 |
| dashboard | 8 | 17 | 5 | 11 | 1 |
| imports-matching | 6 | 21 | 17 | 3 | 1 |
| knowledge | 2 | 4 | 3 | 1 | 0 |
| mac-host | 4 | 11 | 0 | 11 | 0 |
| notes-meetings | 2 | 6 | 3 | 3 | 0 |
| settings | 10 | 24 | 8 | 16 | 0 |
| todoist | 2 | 5 | 0 | 5 | 0 |

Two caveats on the raw numbers, both stated honestly rather than smoothed over. First, **`mac-host` (11 items) is not a Playwright surface at all** — MAC-041/043/044/045 describe macOS notifications and daemon CLI commands (`status`, `doctor`, `configure`, `install`). They are deterministic, but their home is the Swift suite (`mac-daemon/Tests/CRMMacLifecycleTests/{StatusAnarlogTests,DoctorAnarlogTests}.swift` etc.), not `frontend/tests/e2e/`. They are also not toured and not in `classification.ts`, so they are irrelevant to the migration decision; they are counted as NEEDS-NEW-E2E only because the bucket vocabulary has no better slot. Excluding them, the split over the 142 web-surface items is 64 / 75 / 3. Second, **26 of the 64 ALREADY-COVERED items are partial** — the E2E asserts the item's main observable outcome but not every clause (e.g. "disabled at the boundaries" is asserted at the *first* position only). Those are marked `(partial)` in the table with the specific gap named; they still count as covered because the same observable outcome is asserted, but they are the natural place to strengthen an assertion rather than write a new spec.

## Reconciliation with `classification.ts`

`classification.ts` holds exactly 60 rows (contacts 23, dashboard 15, cadence-followup 22 — the `current`-ux items of the three toured domains). 2 are `judge`, 58 are `verifier`.

| classification.ts assignment | Audit bucket | Count |
|---|---|---|
| `verifier` | ALREADY-COVERED (**pure duplication**) | **24** |
| `verifier` | NEEDS-NEW-E2E (real coverage E2E lacks — keep until migrated) | **34** |
| `verifier` | KEEP-FOR-JUDGE (misclassified as deterministic) | **0** |
| `judge` | KEEP-FOR-JUDGE (defensible) | 2 |

Both `judge` rows — CON-042[0] ("the prompt warns the action cannot be undone") and DSH-004[2] ("the shown failure reason faithfully reflects the actual failure") — are correctly judged; each is a claim about the *meaning* of copy, not its presence.

**No verifier row is secretly a judge item.** Every one of the 58 grades a DOM/URL/network fact. That is the finding that settles the migration question: the verifier lane is not a lane of hard-to-test semantics, it is a lane of ordinary E2E assertions expressed against captured aria snapshots.

The 34 NEEDS-NEW-E2E verifier rows are the ones with real value today — but **11 of those 58 verifier rows carry a `caveat` that ends in "→ abstain", meaning they grade nothing at all**: CAD-028[2], CAD-029[2], CAD-030[0], CAD-030[1], CAD-030[2], CAD-031[2], CAD-033[0], CAD-033[1], DSH-005[1], DSH-005[2], DSH-005[3]. Roughly a fifth of the verifier lane is inert. Of those 11, one (CAD-028[2]) is *already proven by E2E* — see Notable findings.

## The table

`grader` column: the `classification.ts` assignment (`—` = not in classification, i.e. not a toured domain or a `proposed` behavior).

### cadence-followup

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| CAD-026 | 0 | overdue contacts appear as cards with the count in the header | verifier | ALREADY-COVERED (partial) | `frontend/tests/e2e/overdue-contact-updates.spec.ts:80` (Action Required header) + `:83` (card present). The numeric *count* is never asserted. |
| CAD-026 | 1 | each card shows urgency tier, cadence, recency, methods, suggested action | verifier | NEEDS-NEW-E2E | `overdue-contact-updates.spec.ts:309-314` asserts these on the **API payload**, not the card. No DOM assertion exists. |
| CAD-026 | 2 | nothing overdue → all-caught-up state | verifier | NEEDS-NEW-E2E | `dashboard.spec.ts:44-53` is an `hasOverdue \|\| hasCaughtUp` or-assert — it passes either way and proves nothing. Needs a route-empty spec. |
| CAD-027 | 0 | urgency (default) orders most-overdue first | verifier | NEEDS-NEW-E2E | No dashboard sort E2E at all. |
| CAD-027 | 1 | name orders alphabetically | verifier | NEEDS-NEW-E2E | — |
| CAD-027 | 2 | last-contacted oldest first, never-contacted last | verifier | NEEDS-NEW-E2E | — |
| CAD-028 | 0 | mutual interaction logged, server-timestamped | verifier | ALREADY-COVERED | `dashboard.spec.ts:139-141` (request AND persisted response are `direction: mutual`) + `overdue-contact-updates.spec.ts:105-109` (server timestamp, full precision, not midnight). |
| CAD-028 | 1 | leaves overdue without reload; count updates | verifier | ALREADY-COVERED (partial) | `dashboard.spec.ts:118-145` (live refetch excludes the id, no nav) + `:148` (card vanishes). The count *number* is not asserted. |
| CAD-028 | 2 | consistent across dashboard, list, and detail | verifier (abstain) | ALREADY-COVERED | `overdue-contact-updates.spec.ts:173-208` — "all views should show consistent state after marking as contacted". The tour abstains on an item E2E already proves. |
| CAD-029 | 0 | last outreach shown when it exists | verifier | ALREADY-COVERED | `contact-direction.spec.ts:44` |
| CAD-029 | 1 | last response shown when it exists | verifier | ALREADY-COVERED | `contact-direction.spec.ts:45` |
| CAD-029 | 2 | awaiting-reply indicator while follow-up pends | verifier (abstain) | NEEDS-NEW-E2E | `contact-direction.spec.ts:126-127` asserts `has_pending_followup === false` via API; the indicator is never rendered or asserted. |
| CAD-029 | 3 | none → explicit no-recent-activity state | verifier | NEEDS-NEW-E2E | — |
| CAD-030 | 0 | follow-up tasks first with pending indicator, then manual | verifier (abstain) | NEEDS-NEW-E2E | `contact-tasks.spec.ts` wraps everything in `if (buttonCount > 0)` — with no Todoist provider it asserts nothing. |
| CAD-030 | 1 | each task badge derived from kind + lifecycle | verifier (abstain) | NEEDS-NEW-E2E | — |
| CAD-030 | 2 | completed tasks collapsed behind a toggle with count | verifier (abstain) | NEEDS-NEW-E2E | — |
| CAD-030 | 3 | no tasks → empty state invites adding | verifier | NEEDS-NEW-E2E | — |
| CAD-031 | 0 | kind chosen from reach-out / send / reminder | verifier | NEEDS-NEW-E2E | — |
| CAD-031 | 1 | task text required; notes optional | verifier | NEEDS-NEW-E2E | `contact-tasks.spec.ts:80-82` asserts submit-disabled, but inside the `if (buttonCount > 0)` no-op guard. Not coverage. |
| CAD-031 | 2 | created task appears in live tasks | verifier (abstain) | NEEDS-NEW-E2E | — |
| CAD-033 | 0 | CRM offers unlink (confirm); task stays alive remotely | verifier (abstain) | NEEDS-NEW-E2E | — |
| CAD-033 | 1 | complete/dismiss happen in the remote app, not the CRM | verifier (abstain) | NEEDS-NEW-E2E | Absence claim; deterministic (assert no complete/dismiss affordance on a linked task row). |

### calendar

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| CAL-024 | 0 | Meetings section shown when contact has ≥1 event | — | ALREADY-COVERED | `meetings.spec.ts:49` |
| CAL-024 | 1 | nothing shown with no events / on load failure | — | ALREADY-COVERED (partial) | `meetings.spec.ts:143` (no events). The load-failure branch is untested. |
| CAL-025 | 0 | three filters (all/upcoming/past) with live counts | — | ALREADY-COVERED | `meetings.spec.ts:52-54` |
| CAL-025 | 1 | upcoming is the default view | — | ALREADY-COVERED | `meetings.spec.ts:56-60` |
| CAL-025 | 2 | past = end time before accelerated current time | — | ALREADY-COVERED (partial) | `meetings.spec.ts:99-103` proves past/upcoming classification; the *accelerated-clock* dependency is not asserted. |
| CAL-025 | 3 | past meetings ordered most-recent-first | — | NEEDS-NEW-E2E | — |
| CAL-026 | 0 | card shows title (fallback when untitled), date, time range | — | NEEDS-NEW-E2E | Only the title text is asserted anywhere; date, range, and the untitled fallback are untested. |
| CAL-026 | 1 | location shown when the meeting has one | — | NEEDS-NEW-E2E | — |
| CAL-026 | 2 | attendee count shown only when > 1 attendee | — | NEEDS-NEW-E2E | — |
| CAL-026 | 3 | past meeting de-emphasized and carries a past marker | — | ALREADY-COVERED (partial) | `meetings.spec.ts:106` (Past badge). "Visually de-emphasized" is a qualitative claim — that clause is judge-material if split out. |
| CAL-027 | 0 | title becomes an external link (new tab) when a source link exists | — | ALREADY-COVERED | `meetings.spec.ts:124-134` (`target="_blank"` + href) |
| CAL-027 | 1 | a meeting without a link renders as plain text | — | NEEDS-NEW-E2E | — |
| CAL-028 | 0 | fixed initial page + control reporting how many remain | — | ALREADY-COVERED (partial) | `meetings.spec.ts:160` (Load more visible after 15 events). The "how many remain" text is not asserted. |
| CAL-028 | 1 | each activation reveals more; control disappears when exhausted | — | ALREADY-COVERED | `meetings.spec.ts:163-167` |
| CAL-028 | 2 | switching filters resets the reveal | — | NEEDS-NEW-E2E | — |
| CAL-029 | 0 | calendar sync control shows state and triggers on demand | — | NEEDS-NEW-E2E | `settings.spec.ts` covers Gmail/Chat badges only, and conditionally. |
| CAL-029 | 1 | triggered sync reports start; reflects progress/errors as it polls | — | NEEDS-NEW-E2E | — |
| CAL-030 | 0 | staleness banner names Google Calendar among stalled sources | — | NEEDS-NEW-E2E | — |
| CAL-030 | 1 | banner quiet while loading / when nothing is stalled | — | NEEDS-NEW-E2E | — |

### contacts

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| CON-038 | 0 | list defaults to cadence order, most frequent first | verifier | NEEDS-NEW-E2E | `contacts.spec.ts:614-651` *comments* that cadence-desc is the default but never asserts row order or request params. |
| CON-038 | 1 | detail prev/next uses the same default | verifier | NEEDS-NEW-E2E | All of `contact-navigation.spec.ts` passes explicit `?sort=name&order=asc`; the default is never exercised. |
| CON-040 | 0 | left/right arrows move prev/next, disabled at the boundaries | verifier | ALREADY-COVERED (partial) | `contact-navigation.spec.ts:102` (prev disabled at first) + `:106` (next enabled). The *last*-position boundary is untested, and arrow-driven movement is only weakly asserted (`:47-48` checks the URL still contains `/contacts/`). |
| CON-040 | 1 | arrows inert while editing or while focus is in an input | verifier | ALREADY-COVERED | `contact-navigation.spec.ts:135-136` (buttons disabled in edit mode) + `:142` (URL unchanged after ArrowRight) + `:178` (URL unchanged with focus in the name input). |
| CON-040 | 2 | Enter opens edit mode | verifier | NEEDS-NEW-E2E | No E2E presses Enter on the detail page. |
| CON-040 | 3 | Escape discards edit, or returns to the list (context preserved) | verifier | ALREADY-COVERED (partial) | `contact-navigation.spec.ts:265` (Escape → Contacts list) + `:300-302` (sort/search restored). The *discard-edit-mode* branch is untested. |
| CON-041 | 0 | the action runs once (edit opens, or merge modal opens) | verifier | ALREADY-COVERED | `contacts.spec.ts:249-252` (`action=edit` → Edit Contact heading) + `:277-280` (`action=merge` → Merge Contacts heading). |
| CON-041 | 1 | the parameter is stripped from the URL | verifier | NEEDS-NEW-E2E | Both tests `waitForURL(/action=edit/)` and stop; nothing asserts the param is later removed. |
| CON-042 | 0 | confirmation prompt warns the action cannot be undone | **judge** | KEEP-FOR-JUDGE | Defensible: a claim about what the copy *means*. |
| CON-042 | 1 | only on confirmation is the contact deleted | verifier | NEEDS-NEW-E2E | There is **no contact-delete E2E anywhere in the suite**. |
| CON-042 | 2 | on success the user is returned to the contact list | verifier | NEEDS-NEW-E2E | — |
| CON-043 | 0 | current marked kept; source picked from a selector excluding the target | verifier | ALREADY-COVERED (partial) | `contact-merge.spec.ts:38` + `:64` (Keeping badge). That the selector *excludes the target* is not asserted. |
| CON-043 | 1 | selecting a source loads a preview | verifier | ALREADY-COVERED | `contact-merge.spec.ts:107` ("Will Be Merged") |
| CON-043 | 2 | conflicting fields toggle, defaulting to keep the target | verifier | ALREADY-COVERED (partial) | `contact-merge.spec.ts:197-205` (toggle works, source becomes selected). The *default-to-target* initial state is not asserted. |
| CON-043 | 3 | merged name editable, with source quick-fill | verifier | ALREADY-COVERED | `contact-merge.spec.ts:243` (inline edit) + `:447-454` ("use this" quick-fill adopts the source name). |
| CON-043 | 4 | cannot submit before source / while preview loads / while merge in flight | verifier | ALREADY-COVERED (partial) | `contact-merge.spec.ts:478` (disabled with no source). The preview-loading and in-flight branches are untested. |
| CON-043 | 5 | the outcome is reported and auto-dismissed | verifier | ALREADY-COVERED (partial) | `contact-merge.spec.ts:396` ("merged successfully") + `:399` (modal closed). Auto-dismissal is untested — but it is deterministic, not judge-material (see Notable findings). |
| CON-044 | 0 | mutual interaction logged from the list row action | verifier | NEEDS-NEW-E2E | `contacts.spec.ts:219` only asserts the "Mark as Contacted" menuitem is *visible*; no test clicks it. The dashboard and detail paths are covered; the **list** path is not. |
| CON-045 | 0 | grouped into today / upcoming / already-celebrated | verifier | ALREADY-COVERED (partial) | `birthdays.spec.ts:96-104` (Today's Birthdays section + card). The upcoming and celebrated sections are never asserted. |
| CON-045 | 1 | gift-planning section appears only near year end | verifier | NEEDS-NEW-E2E | — |
| CON-045 | 2 | upcoming sorts soonest-first; celebrated sink to the end | verifier | NEEDS-NEW-E2E | — |
| CON-045 | 3 | placeholder-year birthdays display without an age | verifier | ALREADY-COVERED | `birthdays.spec.ts:105` (`not.toContainText(/Turning\|Turned/)`) |
| CON-045 | 4 | the page follows the app's accelerated time | verifier | ALREADY-COVERED | `birthdays.spec.ts:12-25` reads `/system/time` and derives "today" from the accelerated clock; `:104` asserts the card lands in the Today section. |
| CON-046 | 0 | failed quick action shows an error rather than silent inaction | — (`proposed`) | NEEDS-NEW-E2E | `status: proposed` — a known gap, not current behavior. |

### dashboard

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| DSH-001 | 0 | root resolves to the dashboard | verifier | ALREADY-COVERED | `dashboard.spec.ts:9` (`toHaveURL('/dashboard')`) |
| DSH-002 | 0 | wide viewports: links to dashboard/contacts/birthdays/imports/settings | verifier | ALREADY-COVERED (partial) | `dashboard.spec.ts:18-19` (Dashboard, Contacts) + `navigation.spec.ts:54` (Imports, on every main page). Birthdays and Settings links are never asserted present. |
| DSH-002 | 1 | the active link is visually marked | verifier | NEEDS-NEW-E2E | The verifier reads `fields.activeNavClass` — exactly a `toHaveClass` assertion, and E2E has none. |
| DSH-002 | 2 | nav stays visible while scrolling (sticky) | verifier | ALREADY-COVERED | `navigation.spec.ts:81-87` (still visible + pinned to top after scroll) + `:96-98` (`sticky top-0 z-50`). |
| DSH-003 | 0 | add-contact action always available from the header | verifier | NEEDS-NEW-E2E | — |
| DSH-003 | 1 | caught-up state offers add-contact + view-full-list | verifier | NEEDS-NEW-E2E | — |
| DSH-004 | 0 | while loading, placeholder content (not empty/caught-up) | verifier | NEEDS-NEW-E2E | Deterministic via `page.route` hold. |
| DSH-004 | 1 | on failure, an error state with a reason (not empty/caught-up) | verifier | NEEDS-NEW-E2E | Deterministic via `page.route` 500. |
| DSH-004 | 2 | the shown failure reason faithfully reflects the actual failure | **judge** | KEEP-FOR-JUDGE | Defensible. |
| DSH-005 | 0 | overdue refreshes without a manual reload | verifier | ALREADY-COVERED | `dashboard.spec.ts:118-145` (the open page's own refetch no longer contains the id) + `:148`. |
| DSH-005 | 1 | covers interaction, merge, and meeting-note-resolve triggers | verifier (abstain) | NEEDS-NEW-E2E | Only the interaction trigger is covered (above). Merge and meeting-note triggers are untested. |
| DSH-005 | 2 | cosmetic edits do not disturb the list | verifier (abstain) | NEEDS-NEW-E2E | — |
| DSH-005 | 3 | refocus refetches only once stale (5-min staleTime) | verifier (abstain) | NEEDS-NEW-E2E | The tour calls this "not deterministically tourable"; Playwright's `page.clock` + request counting makes it deterministic. |
| DSH-006 | 0 | failed dashboard mark-as-contacted shows an error | — (`proposed`) | NEEDS-NEW-E2E | `status: proposed`. |
| DSH-007 | 0 | contact text search via the contact list's search input | verifier | ALREADY-COVERED | `contacts.spec.ts:446-451` (fill + Enter narrows the list) |
| DSH-007 | 1 | no dashboard-level or app-global search surface | verifier | NEEDS-NEW-E2E | Absence assertion; deterministic. |
| DSH-009 | 0 | import-link / cadence-changing edits should refresh overdue | — (`proposed`) | NEEDS-NEW-E2E | `status: proposed` — a known invalidation bug. |

### imports-matching

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| IMP-026 | 0 | People tab: suggestions on top, ranked candidates, source filters, pagination | — | ALREADY-COVERED (partial) | `imports-suggestions.spec.ts:35-36` (suggestion card) + `imports-page.spec.ts:84-90` (source filters, All default) + `imports-features.spec.ts:29-31` (pagination) + `:246-254` (confidence ranking). "On top" as a layout claim is not asserted. |
| IMP-026 | 1 | Interactions tab: conflicts + orphans, attention badge counts only those | — | ALREADY-COVERED (partial) | `imports-interactions.spec.ts:52-57` (tab selected, orphan cards). The badge's discovery-item exclusion is untested. |
| IMP-026 | 2 | manual sync triggers for contact and calendar sources | — | ALREADY-COVERED (partial) | `imports-page.spec.ts:23` (Sync Contacts). The calendar trigger is untested. |
| IMP-026 | 3 | low-confidence names section only when title tokens exist | — | ALREADY-COVERED (partial) | `imports-name-candidates.spec.ts:51-54` (section renders with tokens). The "only when" absence branch is untested. |
| IMP-027 | 0 | everything in one scrollable view — no multi-step wizard | — | KEEP-FOR-JUDGE | A structural/qualitative claim about the layout. Assertable only as a brittle negative ("no step indicator"); genuinely judge-shaped. |
| IMP-027 | 1 | import-as-new vs link-to-existing; link-only sources locked to link | — | ALREADY-COVERED | `imports-actions.spec.ts:117` (mode toggle) + `imports-suggestions.spec.ts:107-108` (link-only: both toggle buttons absent). |
| IMP-027 | 2 | name editable inline; empty name blocks; unresolved telegram peer needs an edit | — | ALREADY-COVERED (partial) | `imports-modal.spec.ts:517-524` (click-to-edit shows input with the name). Empty-name-blocks and the telegram-peer rule are untested. |
| IMP-027 | 3 | methods individually selectable, ≤1 primary; link mode to-add vs present | — | ALREADY-COVERED (partial) | `imports-modal.spec.ts:808-817` (exactly one primary at a time) + `imports-features.spec.ts:414-415` (method rows in link mode) + `imports-suggestions.spec.ts:153-158` (deselect-all). Already-present methods being un-reselectable is untested. |
| IMP-027 | 4 | cadence choosable; link mode pre-fills from the existing contact | — | ALREADY-COVERED | `imports-modal.spec.ts:238-243` (selector present) + `:296-300` and `:360` (prefilled from the target's cadence). |
| IMP-028 | 0 | pager + arrow keys (inert while typing), independent of list pagination | — | ALREADY-COVERED (partial) | `imports-modal.spec.ts:96-123` (ArrowRight/ArrowLeft move between candidates). Inert-while-typing and the 1000-candidate bound are untested. |
| IMP-028 | 1 | after resolving, the modal advances; closes only when the queue is exhausted | — | NEEDS-NEW-E2E | — |
| IMP-028 | 2 | the suggested match is pre-selected in link mode | — | ALREADY-COVERED | `imports-features.spec.ts:154-158` (Link Contact enabled without an explicit selection) |
| IMP-029 | 0 | link action names the suggested contact with its confidence percentage | — | ALREADY-COVERED | `imports-features.spec.ts:109-114` |
| IMP-029 | 1 | without a suggestion, the link action asks the user to select | — | ALREADY-COVERED | `imports-features.spec.ts:69` ("Link (select)") |
| IMP-029 | 2 | import is not offered on link-only sources | — | ALREADY-COVERED | `imports-suggestions.spec.ts:97` + `imports-correspondence.spec.ts:56` |
| IMP-030 | 0 | target contact fixed — no contact selection, no import mode | — | ALREADY-COVERED | `imports-suggestions.spec.ts:40-42` |
| IMP-030 | 1 | methods already on the contact shown disabled | — | NEEDS-NEW-E2E | — |
| IMP-030 | 2 | confirm requires ≥1 method; dismiss dismisses all pending, stickily | — | ALREADY-COVERED (partial) | `imports-suggestions.spec.ts:65-71` (dismiss survives reload). The ≥1-method requirement is untested. |
| IMP-031 | 0 | the item leaves its queue and dependent counts update | — | ALREADY-COVERED (partial) | `imports-actions.spec.ts:143` (import), `:219` (ignore), `:331` (link) — each card leaves the list. Count updates are untested. |
| IMP-031 | 1 | dependent surfaces refresh through scoped query invalidation | — | NEEDS-NEW-E2E | `imports-modal.spec.ts:381-398` re-reads the contact after a full navigation, which does not prove invalidation. |
| IMP-031 | 2 | a returned rematch job is registered for polling | — | ALREADY-COVERED (partial) | `imports-actions.spec.ts:137-140` (response carries `rematch_job_id`). The frontend *registering* it for polling is untested. |

### knowledge

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| KNW-034 | 0 | known location → labeled row; unknown → no row | — | NEEDS-NEW-E2E | `contacts.spec.ts:46-84` covers the list-table truncation, not the detail row or its absence. |
| KNW-034 | 1 | known birthday → labeled date row; unknown → no row | — | ALREADY-COVERED (partial) | `birthdays.spec.ts:91` (detail shows the date). The no-row-when-unknown branch is untested. |
| KNW-035 | 0 | month/day only; the placeholder year is never shown | — | ALREADY-COVERED | `birthdays.spec.ts:84-87` (list) + `:92` (detail: `1900` not visible) |
| KNW-035 | 1 | no age is computed or displayed | — | ALREADY-COVERED | `birthdays.spec.ts:105` |

### mac-host

Not a Playwright surface — these describe macOS notifications and daemon CLI commands. Deterministic, but their home is the Swift suite (`mac-daemon/Tests/`), not `frontend/tests/e2e/`. None are toured or in `classification.ts`, so they do not bear on the migration.

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| MAC-041 | 0 | orphan prompts tagging participants; conflict prompts resolving in imports | — | NEEDS-NEW-E2E | Not web-reachable; Swift daemon test. The copy claim is judge-shaped if ever toured. |
| MAC-041 | 1 | tapping an orphan opens the note app; a conflict opens the scoped imports page | — | NEEDS-NEW-E2E | Not web-reachable (deep-link target `/imports?session=` *is* covered by `imports-interactions.spec.ts:115-121`). |
| MAC-041 | 2 | notifications for resolved sessions removed on the next reconcile | — | NEEDS-NEW-E2E | Not web-reachable; Swift daemon test. |
| MAC-041 | 3 | an unrecognized linkage state is dropped without crashing | — | NEEDS-NEW-E2E | Not web-reachable; Swift daemon test. |
| MAC-043 | 0 | status reports install/registration/pairing + watermarks, no network call | — | NEEDS-NEW-E2E | Not web-reachable; `mac-daemon/Tests/CRMMacLifecycleTests/StatusAnarlogTests.swift` is the right home. |
| MAC-043 | 1 | doctor runs health checks and exits non-zero by failing-check count | — | NEEDS-NEW-E2E | Not web-reachable; `DoctorAnarlogTests.swift`. |
| MAC-044 | 0 | ops commands refuse to run against a live daemon | — | NEEDS-NEW-E2E | Not web-reachable. |
| MAC-044 | 1 | backfill/scan commit via the same cursor CAS; local state mirrors it | — | NEEDS-NEW-E2E | Not web-reachable. |
| MAC-045 | 0 | install pairs, registers launchd, provisions permission + allowlist | — | NEEDS-NEW-E2E | Not web-reachable. |
| MAC-045 | 1 | plaintext non-loopback Pi URL warns but does not block | — | NEEDS-NEW-E2E | Not web-reachable. |
| MAC-045 | 2 | re-pair rotates the key; a persist failure prints the plaintext key | — | NEEDS-NEW-E2E | Not web-reachable. (The *web* side of re-pair — the rotate-key modal — is covered by `settings-mac.spec.ts:303-318`.) |

### notes-meetings

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| NTS-007 | 0 | notepad appears only when it has content | — | NEEDS-NEW-E2E | `contacts.spec.ts:152` shows a populated notepad; the empty/absent → nothing branch is untested. |
| NTS-007 | 1 | the body preserves the user's line breaks | — | NEEDS-NEW-E2E | — |
| NTS-007 | 2 | long content clamped; expand/collapse appears only on overflow | — | ALREADY-COVERED | `contacts.spec.ts:116` / `:123` / `:129` (Show more ↔ Show less round-trip) + `:155` (no button for short notes). |
| NTS-008 | 0 | notepad saved as its own operation; both complete before edit closes | — | ALREADY-COVERED | `contacts.spec.ts:547-553` (submit → edit mode closes → new notes rendered) |
| NTS-008 | 1 | clearing the notepad field removes the note | — | NEEDS-NEW-E2E | — |
| NTS-008 | 2 | the displayed notepad reflects the new content after save | — | ALREADY-COVERED | `contacts.spec.ts:553` (+ `:554`: the old text is gone) |

### settings

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| SET-019 | 0 | each provider has a section showing connection state | — | ALREADY-COVERED (partial) | `settings.spec.ts:9` (Todoist) + `:30` (Google Accounts) + `telegram-settings.spec.ts:9` (Telegram). Section *presence* is asserted; connection state only via or-asserts. |
| SET-019 | 1 | a connected account shows its identity and when it was connected | — | ALREADY-COVERED (partial) | `telegram-settings.spec.ts:280-281` (username + phone, mocked). "When it was connected" is never asserted. |
| SET-019 | 2 | each section offers connect and disconnect affordances | — | ALREADY-COVERED (partial) | `telegram-settings.spec.ts:282` (Disconnect button). Todoist's connect button is behind an or-assert (`settings.spec.ts:20`). |
| SET-020 | 0 | app fetches the provider auth URL and navigates the whole page to consent | — | NEEDS-NEW-E2E | — |
| SET-021 | 0 | success outcome → success indication + account list refresh | — | NEEDS-NEW-E2E | — |
| SET-021 | 1 | error outcome → the failure reason is shown | — | NEEDS-NEW-E2E | — |
| SET-021 | 2 | one-time outcome params stripped from the URL | — | NEEDS-NEW-E2E | Same class as CON-041[1]; deterministic. |
| SET-022 | 0 | confirmation identifies the account and warns what access is revoked | — | NEEDS-NEW-E2E | The *copy* clause is judge-shaped; the prompt's presence + account identity are deterministic. |
| SET-022 | 1 | only on confirmation is the account revoked | — | NEEDS-NEW-E2E | `telegram-settings.spec.ts:285-290` explicitly documents omitting the disconnect test (route-glob collision). |
| SET-022 | 2 | outcome reported; the account list reflects the removal | — | NEEDS-NEW-E2E | — |
| SET-023 | 0 | unconfigured/errored section → not-connected empty state, not an error stack | — | ALREADY-COVERED (partial) | `telegram-settings.spec.ts:28-32` ("Configuration Required" or "Connect Telegram"). Only Telegram; Todoist/Google use pass-either-way or-asserts. |
| SET-023 | 1 | the section lists the configuration the deployment must supply | — | NEEDS-NEW-E2E | `settings.spec.ts:16-20` is an `hasConnectButton \|\| hasConfigMessage` or-assert — passes with neither branch proven. |
| SET-024 | 0 | per-source sync affordance appears only when the scope is held | — | NEEDS-NEW-E2E | `settings.spec.ts:41-46` is guarded by `if (hasGmailBadge)` — a no-op in the default E2E env. |
| SET-024 | 1 | Chat affordance only on full chat scopes; partial grant → reconnect prompt | — | NEEDS-NEW-E2E | `settings.spec.ts:60-81` — same conditional no-op shape. |
| SET-025 | 0 | auth-error sync state → a reconnect affordance appears | — | NEEDS-NEW-E2E | — |
| SET-025 | 1 | prompt suppressed once the credential is newer than the failing sync state | — | NEEDS-NEW-E2E | — |
| SET-026 | 0 | project and label pickers populated from live provider lists | — | NEEDS-NEW-E2E | — |
| SET-026 | 1 | selecting a project or label persists and reports the outcome | — | NEEDS-NEW-E2E | — |
| SET-026 | 2 | surface indicates both are needed before cadence sync is active | — | NEEDS-NEW-E2E | — |
| SET-027 | 0 | the Telegram section is present on settings | — | ALREADY-COVERED | `telegram-settings.spec.ts:9` |
| SET-027 | 1 | feature off → not-configured state naming the required configuration | — | ALREADY-COVERED (partial) | `telegram-settings.spec.ts:28-32`. The *naming* of the required config is not asserted. |
| SET-028 | 0 | export affordance downloads a JSON backup | — | ALREADY-COVERED (partial) | `settings.spec.ts:92-93` (Export Data heading + Download Backup button). The download itself is never triggered. |
| SET-028 | 1 | import affordance accepts a backup file | — | ALREADY-COVERED | `settings.spec.ts:99-101` (file input, `accept=".json"`) |
| SET-028 | 2 | the surface communicates that import does not yet modify stored data | — | NEEDS-NEW-E2E | — |

### todoist

| behavior_id | then_idx | then (abbreviated) | grader | bucket | citation / note |
|---|---|---|---|---|---|
| TDS-034 | 0 | manual task sync requested for the account; success/failure indicated | — | NEEDS-NEW-E2E | — |
| TDS-034 | 1 | the account's read/write permission state is visible | — | NEEDS-NEW-E2E | — |
| TDS-035 | 0 | CRM markers stripped; placeholder fallback when empty | — | NEEDS-NEW-E2E | — |
| TDS-035 | 1 | each linked task offers a deep link into the Todoist app | — | NEEDS-NEW-E2E | — |
| TDS-035 | 2 | the cadence-due projection is not surfaced on the contact page | — | NEEDS-NEW-E2E | Absence claim; deterministic. |

## Notable findings

**The clearest duplications — verifier rows that a Playwright spec already asserts, line for line.** DSH-002[2] (sticky nav) is graded from `fields.navPosition === 'sticky'` (a computed style read out of the capture) while `navigation.spec.ts:96-98` asserts `toHaveClass(/sticky/, /top-0/, /z-50/)` and `:81-87` asserts the nav is still pinned to the viewport top after a 500px scroll — the E2E is strictly stronger. DSH-001[0] (root → `/dashboard`) is one `toHaveURL` in `dashboard.spec.ts:9`. CAD-028[0] and CAD-028[1] are asserted in `dashboard.spec.ts:118-148` with a *content-based* response predicate that is more careful about the refetch race than the capture-diff approach. CON-040[1] (arrows inert while editing / in an input) is `contact-navigation.spec.ts:135-178`. CON-043[1]/[3] (preview loads, name editable with quick-fill) are `contact-merge.spec.ts:107` and `:243`/`:447-454`. CON-045[3]/[4] (no age on placeholder-year birthdays, page follows accelerated time) are `birthdays.spec.ts:105` and `:12-25`. In every one of these the tour re-derives from an aria snapshot what an existing spec already asserts against the live DOM.

**CAD-028[2] is the sharpest case: the tour abstains on an item E2E already proves.** Its classification note reads "Multi-surface (dashboard/list/detail) consistency is not toured in one flow → abstain" — meanwhile `overdue-contact-updates.spec.ts:173-208` is a test literally titled "all views should show consistent state after marking as contacted". The verifier lane is carrying a row that grades nothing, for a behavior that is already green in CI.

**Nothing the verifier grades is beyond E2E's reach.** I looked specifically for items where the capture-based verifier can see something Playwright cannot, and found none — all 58 verifier rows are DOM, URL, or network facts. The direction of the gap runs the other way: DSH-005[3] (refocus refetches only once the 5-minute staleTime has elapsed) is marked "not deterministically tourable → abstain", yet Playwright's `page.clock` plus request counting makes it straightforwardly deterministic. Same for DSH-004[0]/[1] (loading and error states), which no tour can reach without a route interceptor but `page.route` handles trivially. **E2E is the more capable harness for every item in the verifier lane.**

**11 of the 58 verifier rows (19%) abstain — they grade nothing today.** CAD-028[2], CAD-029[2], CAD-030[0-2], CAD-031[2], CAD-033[0]/[1], DSH-005[1]/[2]/[3]. Most abstain because the flow needs a seeded Todoist provider that a provider-less tour sweep cannot reach. That is a seeding problem, and E2E already has the seeding infrastructure (`testApi.seed*`) to solve it.

**One item is misgraded toward the judge: CON-043[5].** Its note says the success banner is "a copy anchor — unbindable → unbound → judge", so at grade time it routes to the LLM. But the observable outcome — *the outcome is reported and auto-dismissed* — is fully deterministic, and `contact-merge.spec.ts:396`/`:399` already asserts the reporting half. Only the auto-dismissal is unasserted, and `toBeHidden({ timeout })` covers it. This item belongs in E2E, not the judge.

**Both existing `judge` rows are correctly placed, and I found three more genuinely judge-shaped items outside the toured domains.** CON-042[0] and DSH-004[2] are right. Beyond them: IMP-027[0] ("everything is one scrollable view — no multi-step wizard") is a structural claim assertable only as a brittle negative; CAL-026[3]'s "visually de-emphasized" clause (the "past marker" half is already covered at `meetings.spec.ts:106`); and SET-022[0]'s "warns what access is revoked" clause. The judge residue is real but small — on the order of 3-6 items across the whole 153, which matches the design intent of "the judge residue is deliberately tiny" far better than the current 2-of-60 split implies, because the current split is tiny for the wrong reason (the other 58 are E2E work misfiled as grading).

**Two coverage traps that inflate the apparent E2E baseline.** First, `contact-tasks.spec.ts` wraps every assertion in `if (buttonCount > 0)` and `settings.spec.ts:41-81` does the same with `if (hasGmailBadge)` / `if (hasChatBadge)` — with no provider configured in the E2E env these tests pass while asserting nothing. They are the E2E equivalent of the tours' abstains. Second, `dashboard.spec.ts:44-53` asserts `hasOverdue || hasCaughtUp`, which is true in every possible state. I counted all three as NEEDS-NEW-E2E, not coverage.

**The biggest genuine gaps, if the migration wants a priority order.** There is *no contact-delete E2E anywhere in the suite* (CON-042 — three items, one of the riskiest flows in the app). CON-041[1] and SET-021[2] (one-time URL params must be stripped) are never asserted despite being a documented repo gotcha. CON-038[0]/[1] (the default cadence ordering shared by list and detail) is only ever exercised with an explicit sort override. CAD-027 (the three dashboard orderings) has no E2E at all. And `settings` is the thinnest domain overall: 16 of 24 items uncovered, including the entire OAuth connect/disconnect lifecycle (SET-020, SET-021, SET-022).
