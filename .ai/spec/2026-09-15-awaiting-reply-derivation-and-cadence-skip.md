# Awaiting-reply derivation + cadence skip — Spec

**Issues:** #680 (the bug, with the 2026-09-15 decision comment as authoritative scope), #212 (skip/undo-skip), #854 (introduced the second symptom)
**Status:** Approved 2026-09-15 — charter-bearing; arc certified by four contract-review rounds with the round-4 fixes verified; both PRs auto-merge on green CI + merge gate, promotion is the human gate
**Domain spec:** `spec/cadence-followup.yaml`

---

## 1. Problem

The CRM answers "am I waiting to hear back from this contact?" by checking whether a live `contact_task` row with `lifecycle=followup_loop` exists. That row is a Todoist artifact. Deleting the reminder in Todoist therefore rewrites CRM state: the contact jumps back to full urgency on the dashboard and a cadence task is minted at the stale `contact_by`, born overdue, in the same sync tick.

Verified in prod on a contact on a monthly cadence: follow-up dismissed, replacement cadence task created 11 ms later with a deadline six days in the past.

The fact the system needs is already recorded on the contact — the last outbound is more recent than the last response. Four separate places ask the task row instead:

1. the dashboard demotion (`has_pending_followup`, batched in `pendingFollowUpSet`)
2. the cadence-task creation gate (`FindPendingFollowUp` in `reconcileContactTasks`)
3. the contact-list `has_followup` / `no_followup` filter
4. the contact-detail awaiting-reply indicator

There is also no way to say "leave this person alone this cycle" from the CRM. Skip exists only as a Todoist gesture — deleting a cadence task — and during a follow-up thread there is no cadence task to delete. So the only lever available is deleting the follow-up reminder, which is why that gesture accumulated meanings it was never given.

## 2. Why now, and why not the inference work

#680 was deferred on the grounds that follow-up dismissal is ambiguous ("skip this cycle" vs "no follow-up was actually required") and that intent-gated follow-up creation would reshape the lifecycle anyway.

Intent-gating answers whether an obligation *exists*. The observed case is one where it plainly does — real outbound, no response, a follow-up correctly warranted — and the user still wants to set it aside, because the reason is about the user and not about the message. Skip is orthogonal to intent-gating, not superseded by it. The extraction work (#379) remains wanted on its own merits and is out of scope here.

## 3. Intent &amp; appetite (delegation charter)

### Ranked priorities

1. **No Todoist action may change CRM state.** Deleting a follow-up reminder means "stop reminding me to chase this message" and nothing more.
2. **Awaiting-reply is derived from interaction data, with a single writer.** The task row stops answering CRM questions.
3. **One nag at a time.** When the waiting period lapses, the user does not get an overdue follow-up reminder and a fresh cadence task side by side saying the same thing.
4. **Skip is a first-class CRM control**, with undo, so the user never has to express "leave this person alone" through a Todoist deletion.

Priority 1 is the bug. Priorities 2 and 3 are what make it stay fixed. Priority 4 is what makes the fix feel complete rather than merely correct.

### Appetite

Two PRs, one promotion cycle. This is a correctness fix plus a small, already-specced feature — not a cadence-engine redesign. Machinery beyond what the two PRs need is disproportionate: no new gates, no sweepers, no background jobs, no abstraction layers whose only job is forwarding.

The expiry is stored as a **date**, never a boolean, precisely so that no scheduled job is needed to expire it. A design that reintroduces a sweeper has taken a wrong turn.

### Rabbit holes (explicitly out of scope)

- **LLM intent-gating of follow-up creation** (#379). Orthogonal, still wanted, not here.
- **A Todoist-side skip gesture.** Decided against — do not invent a label convention or a task-name protocol.
- **Changing the cadence clock rules.** Outbound still never advances `contact_by`; only inbound, mutual, and an explicit skip do. This invariant is not up for revision.
- **Reworking the `contact_task` kind/lifecycle taxonomy.** Settled by `task-direction-taxonomy.md`.
- **Reworking `FindPendingFollowUp` itself.** It survives unchanged. It keeps two callers: follow-up idempotency and refresh inside `FollowUpManager`, and the reconciler's post-lapse arm, where it answers only the narrow question "does a Todoist reminder already exist for this contact" — never the CRM-state question of whether a reply is awaited.
- **Adjacent cadence bugs** found mid-run (#753, #645, #163 are open and nearby). File, do not fix. #645 in particular is the reason skip's browser-level proof is waived; fixing it is a separate arc.

### Call me when

- Any decision would change **when or by how much `contact_by` advances**, beyond the two sanctioned movements (skip forward one cycle, undo back to the pre-skip value).
- Any change would **reverse an existing `current` spec behavior**, requiring retire-and-mint rather than extend-in-place — this invalidates existing test citations and is a spec-corpus decision, not an implementation one.
- Any fix would **touch files or behavior outside the two PRs as specced**.
- The **derived-writer trigger** would need a new owner value, or any of the existing eight derived columns would need its write path changed.

### User-reserved decisions

- **Promotion to prod.** Always. `make promote` is never autonomous.
- **UI copy and placement beyond what section 5 specifies.**

### Settled — do not re-litigate


| Decision                     | Value                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Expiry representation        | A date on the contact, never a boolean                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| Awaiting-reply test          | `last_outreach_at > last_response_at`, strict                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| Dashboard card copy in PR 1  | Unchanged — no user-visible change ships in the auto-merging PR                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| Undo-skip surface            | Persistent on contact detail, for as long as the skip is in effect                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| Skip scope                   | Full #212 — skip and undo together in PR 2                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| Skip arithmetic              | Reuses the existing `handleSkipTrigger` rule, not a reimplementation                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| Todoist skip gesture         | None                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| Resurrected cadence deadline | Clamped to today or later                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| Skip's cadence unit          | Unified onto the shared duration path used by the other nine cadence call sites; `cadence.CadenceDays` is deleted                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| Undo window                  | Offered even when the restored date is already past; retires when the skipped-to date arrives                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| Card action set              | Log Interaction becomes the primary button, Skip this cycle sits below it at equal width, Mark as Contacted is removed, View details is replaced by a clickable contact name                                                                                                                                                                                                                                                                                                                                                                                                |
| Card redesign scope          | Rides in PR 2 (user-affirmed 2026-09-15)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| UI reference                 | The design canvas is the pinned source for PR 2's surface: [https://claude.ai/artifact/Sp7CAwFKeRGLqLkycrJFMg](https://claude.ai/artifact/Sp7CAwFKeRGLqLkycrJFMg)                                                                                                                                                                                                                                                                                                                                                                                                           |
| Expiry write semantics       | Set forward-only by an outbound, like its five siblings; a response never touches it, so a late-ingested older message cannot end an open window. Two writers may lower it, both already sanctioned for the siblings: a skip sets it NULL (the one CRM action that ends the window without a reply), and the delete-rollback recompute re-derives it from the recomputed `last_outreach_at` via the shared expression                                                                                                                                                       |
| Expiry backfill              | Migration 082 initialises every live cadence-bearing contact from `Today(last_outreach_at) + watchdog(cadence)` under `crm.derived_writer='cadence'`; no event replay                                                                                                                                                                                                                                                                                                                                                                                                       |
| Undo withdrawal              | One setter, every other writer clears: skip state is written only by the skip itself and cleared by every other path that touches the clock — any inbound or mutual interaction on either cadence branch (whether or not `contact_by` moved, because a real conversation makes restoring a pre-skip date wrong regardless), a cadence edit, a Todoist-side skip, and the delete-rollback recompute. Undo is offered while skip state is present and today is before `contact_by`                                                                                            |
| Post-skip Todoist state      | The follow-up row goes to `completed` — the state the create worker already closes remotely in the in-flight case, so an in-progress create still ends with the reminder closed exactly once, and CAD-016's `dismissed` semantics for a Todoist-side deletion stay untouched. With the window ended and the reminder closed, the reconciler creates or re-dates the cadence task at `max(contact_by, today)`; undo restores `contact_by` and the task follows to `max(restored, today)` — the clamp is universal. Undo does not resurrect the reminder or reopen the window |
| E2E gate                     | `make test-e2e-local` with a per-PR `PLAYWRIGHT_GREP`; `make test-e2e-diff` no longer exists                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| Skip proof lane              | Skip, undo, and leaves-the-overdue-set are proven in Go under `cadence.ProductionCadenceConfig()`. The browser proves the controls render and the API fields flip. The two assertions the accelerated lane cannot observe — the card leaving the dashboard, and undo withdrawn when the skipped-to date arrives — are waived per then-item with a reason naming #645 (`contact_by` is DATE-typed; a compressed-cadence skip writes today; the testing overdue path ignores `contact_by` by design). #645 stays open and out of scope                                        |


## 4. Behavioral claims

Reserved ID block: **CAD-040 through CAD-046**; the arc assigns them (CAD-043 stays unspent). Existing behaviors touched, each with its disposition ruled below: **CAD-016** (dismissal semantics), **CAD-018** (cadence-due creation and its follow-up deferral clause), **CAD-019** (skip advances the schedule), **CAD-023** (overdue endpoint's `has_pending_followup` key), **CAD-026** (dashboard demotion), **CAD-028** (marking contacted from the dashboard card), **CAD-029** (detail-page awaiting indicator), **CAD-034** (uniform cadence arithmetic, proposed), **CON-005** (contact payload flag), **CON-018** (follow-up filter vocabulary), **CON-022** (follow-up filter mechanism), **DSH-008** (the dashboard's write surface). **CON-048** (no hard-delete exposed) is unchanged; only its citing test is rescoped in PR 2.

`CAD-018`'s third then-item and `CAD-023`'s `each-entry-carries-pending-followup` key both state the follow-up-row dependency as the rule, so both were candidates for reversal. They were ruled separately below: `CAD-018`'s item stays literally true under the new two-condition gate and needs no edit, while `CAD-023`'s does not and is rewritten. The distinction matters because a keyed then-item edited in place silently retargets every test citing it.

### Spec-corpus rulings (2026-09-15)

- **CAD-023 and CAD-029 are edited in place.** Their reversing then-items are rewritten to describe the derived rule; IDs and keys are kept, and every citing test is updated in the same PR, so no citation is silently retargeted. CAD-043 stays unspent.
- **CAD-028 is retired into CAD-046.** PR 2 removes the one-click mark-as-contacted action, so all three of its then-items become false and `spec/README.md`'s reversal rule applies. DSH-010 and DSH-012 each retain other serving behaviors, so no intent is left without evidence. Its two citing E2E tests are rewritten rather than deleted (each co-cites a surviving behavior), and the `dashboard.tour.ts` captures tagged CAD-028 are re-pointed at the new action set.
- **CAD-016, CAD-018, CAD-019, CAD-026 are extended in place**, none carrying a reversing edit.
- **CON-005 (`live-contact-flag`, `list-entries-carry-flag`) and CON-022 are edited in place** on the same principle as CAD-023: they name the wire field and the task-backed mechanism, every citing test (`direction_api_test.go:192`, `:282`) is updated in the same PR, and the filter's user-visible contract is unchanged. Applied by the coordinator citing this block; the user may override.
- **CAD-034 flips `proposed → current` in PR 2.** It already describes the skip-interval unification; the implementation lands it under the maintenance rule. Not a reversal.
- **CON-018 is extended in place** (PR 1): `followup-filter-closed-set` states the filter's accepted vocabulary, which is unchanged; only what backs the values changes. Its citations stay valid; the test that carries them (`TestContactAPI_FollowupFilter`) needs its fixture repaired in the same PR.
- **DSH-008 is edited in place** (PR 2), as a direct consequence of the CAD-028 retirement: its statement that the dashboard's only write is mark-as-contacted becomes the new write surface (log-interaction through the modal, and skip). The durable intent — the dashboard owns no persisted state — is untouched. It is `type: invariant`, `surface: none`, and nothing cites it.

### Derivation (PR 1)

- The awaiting-reply state holds when the last outreach is strictly later than the last response, and the waiting period has not expired.
- The waiting period expires at `Today(outreach) + watchdog days for the contact's cadence` — the same window that already sets the follow-up reminder's deadline.
- The expiry is recorded on the contact and written only under the cadence owner: set forward-only by an outbound, never touched by a response, ended by a skip, and recomputed alongside its sibling columns when an interaction is deleted.
- No Todoist-side action changes the awaiting-reply state.

### Nagging (PR 1)

- While the waiting period is open, no cadence task is created, whatever exists in Todoist.
- Once it has expired, a cadence task is created only if no live follow-up reminder already covers the contact.
- A cadence task created after an expired waiting period carries a deadline of today or later, never a stale past date.

### Card actions (PR 2)

- Both dashboard card states offer exactly two actions: log an interaction, and skip this cycle.
- The one-click mark-as-contacted action is removed from the dashboard card.
- A card's contact name is the link to that contact; there is no separate view-details link.

### Skip (PR 2)

- Skipping a contact ends any live follow-up thread and advances the next-contact date by exactly one cadence cycle, recording no interaction.
- A skipped contact leaves the dashboard. Its cadence task is re-dated to the skipped-to date rather than suppressed — CAD-018's one-live-task rule is unchanged, and a future-dated task is not a nag.
- The skip is reversible until the skipped-to date arrives, whether or not the restored date has already passed; the control is offered on contact detail and disappears once the skip is no longer in effect. **This overrides #212's rule** that undo is suppressed when the restored date is in the past — restoring an overdue contact to overdue is the state they were in, and the overdue case is the one that motivated this work.
- The skip records when it happened, what the previous next-contact date was, and why (`ui`).

## 5. Acceptance table

The behavior this work must produce end to end. Rows 2 through 4 are currently wrong or unreachable.


| Situation                                       | Dashboard                       | Todoist                                                                      |
| ----------------------------------------------- | ------------------------------- | ---------------------------------------------------------------------------- |
| Reach out                                       | demoted, awaiting reply         | follow-up reminder, no cadence task                                          |
| **Delete the follow-up reminder**               | **unchanged — still demoted**   | **nothing appears**                                                          |
| Waiting period lapses, reminder still live      | full urgency                    | the overdue reminder only — no second task                                   |
| Waiting period lapses, reminder deleted earlier | full urgency                    | cadence task, dated today or later                                           |
| They reply                                      | leaves the list, clock advances | reminder closes                                                              |
| Skip                                            | leaves the list                 | window ended, follow-up reminder closes, cadence task at the skipped-to date |


Row 2 must be proven by a test that fails against `develop` today.

## 6. Spike findings (2026-09-15)

**The skip arithmetic is sound and safe to reuse.** Applied to all 152 cadence-bearing contacts in prod, every cadence produces a forward date, and 151 take the `contact_by + cycle` branch rather than `today + cycle` — so the later-of rule is load-bearing, not decoration: skipping a contact who is not yet due pushes from their due date.

**Skip is the codebase's only day-count cadence.** `cadence.CadenceDays` has exactly one caller — the skip path. The other nine cadence computations (updater, repository, service, enrichment) all resolve a configurable duration that shrinks under `CRM_ENV=test`. The divergence has never mattered because skip was reachable only by deleting a Todoist task, which no test drives; a CRM entry point makes it reachable from a test for the first time, where a monthly cadence would be 10 minutes everywhere except skip. Hence the unification above. Note it is not a pure no-op in production: calendar-day addition and absolute-hour addition differ by a day across a DST boundary. Accepted — `contact_by` is date-precision and cadences are approximate.

**Skip has shipped since the original Todoist integration** (#223). PR 2 adds an entry point, not a behavior.

**Under `CRM_ENV=testing` the overdue list ignores `contact_by`.** The accelerated read path (`service/contact.go` ~890) recomputes overdue-ness from `last_contacted`/`created_at`. Skip moves only `contact_by`, so in the E2E lane a skipped contact would stay on the dashboard, and a compressed-cadence skip writes today into the DATE column so undo is never observable there. Ruled 2026-09-15 (settled row "Skip proof lane"): the clock behavior is proven in Go under production cadence config and the two browser assertions are waived naming #645. Found in arc review round 1; ruling revised after round 3.

## 7. Out of scope

- Every rabbit hole in section 3.
- Surfacing the expiry in the dashboard card (decided: PR 1 ships no visible change).
- Bulk skip, or skip from the contact list.
- Reworking the interaction-logging modal itself. Removing the one-click mark-as-contacted action resolves #470 (undo for it) and most of #347 (consolidating its three call sites) by deletion rather than by work; neither is otherwise in scope.
- Undo for anything other than skip.

