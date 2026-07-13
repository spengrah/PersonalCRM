# Synthetic Seed Fidelity Audit — invariants, classification, staging measurements

**Date:** 2026-07-13. **Trigger:** three consecutive false regressions from the agentic UX QA judge, each traced to the seeded world holding state production cannot produce (missing follow-up state → "feature doesn't exist"; follow-up without an outbound → "awaiting reply to nothing"; `last_contacted` without an interaction → issue #640, closed invalid, refiled as #641). **Question:** are those three the whole story, or the tip of something broader? **Method:** five parallel read-only audits (one per invariant family: cadence timestamps, knowledge/assertions, contact_task, interactions/events, identity/structural), each deriving invariants from the production write paths, classifying every seed write as cause-driven vs endpoint-written, and emitting measurement SQL — then all queries run against the live staging world (167 live contacts, prod-shaped profile, image `58dbbccf`).

**Answer:** the seed's replay architecture is fundamentally sound — every interaction/event row is authored by a real provider, identity matching runs the real matcher, contact/node/merge/soft-delete all go through real services, and 14 of the ~25 measured invariants came back clean. But there are two large, judge-visible coherence breaks (F1, F4), one environment-level surprise that is not a seed bug at all (F2), and a tail of small, cheap-to-fix shape gaps. The three incidents were not isolated: they are all instances of one pattern — **writing a derived endpoint (or a partial causal chain) instead of driving the cause** — and that pattern has more live instances.

## The one-line principle

Seed the cause, not the endpoint. If production derives a value, the seed must either drive the production deriver or reproduce its exact output shape — anything in between produces worlds the judge correctly reports as broken.

## Findings, ranked by (occurrence × judge-visibility)

### F1 — The overdue population is built on production-impossible state (confirms + expands #641)

Measured: **111/114** contacts with `last_contacted` have it *earlier than* `created_at` (production forces creation-time; the handler ignores any client value — `handlers/contact.go:142-160`, CON-001). Worse: **95/114** have a non-creation `last_contacted` with **no backing interaction row at all** (and `last_interaction_at` NULL) — production's own delete-rollback path (CAD-010, `contact.sql:519-542`) proves every non-creation `last_contacted` must equal a live inbound/mutual interaction's `occurred_at`. Root cause: `factory.WithOverdue()`/`WithRecent()` (`factory/domain.go:190-198`) write `spec.LastContacted` raw, and `catalogOptionsFor` (`profiles.go:1002-1023`) applies them to ~2/3 of every catalog contact, deliberately skipping interaction replay for them so the cadence engine won't overwrite the fake value (`profiles.go:296-302`). The judge-facing symptom is severe: "Last contacted 3 months ago" in the header next to an **empty activity timeline** — a structural contradiction requiring no domain knowledge to flag. This population is the entire overdue cohort, i.e. the dashboard's central subject.

Fix direction (cause-driven): overdue = a contact **created long ago** (backdated `created_at`, which the repo layer accepts legitimately) whose creation-time `last_contacted` is therefore old — matching the dominant real-world overdue shape — plus a smaller cohort whose `last_contacted` moved because a **replayed inbound/mutual interaction** occurred at the desired time. Both shapes are production-reachable; neither needs the factory to touch `last_contacted` at all.

### F2 — `CRM_ENV=staging` silently runs hour-scale cadences (environment finding, NOT a seed bug)

Measured: **149/149** cadence-bearing contacts have `contact_by` equal to `last_contacted`'s date instead of `last_contacted + cadence`. Diagnosis: `GetCadenceConfig` (`cadence/cadence_config.go:20-43`) keys off `CRM_ENV`, and `staging` selects the "fast staging" branch — **monthly = 1 hour, weekly = 10 minutes**. Every cadence computation on the staging box, at seed time and at runtime, uses hour-scale durations; with `CalculateContactBy` date-truncating (staging is not `IsTestingMode()`), monthly `contact_by` collapses onto `last_contacted`'s own date. Consequence for QA: the staging world's overdue math — the app's central value loop — does not behave like production's. A judge that checks "monthly cadence + last contacted Apr 14 → due mid-May" will (correctly, per prod semantics) fail the UI, which shows due-same-day. This interacts with F1: today's overdue cohort is overdue for two stacked non-prod reasons.

**Decision (2026-07-13): staging runs production cadence semantics.** The `staging`/`accelerated` alias pair is consumed in exactly two places, both in `cadence/cadence_config.go`: `GetCadenceConfig` (durations) and the overdue-days scaler (which on staging computes one "day" as 10min/7, inflating displayed overdue-ness ~1000×). Nothing else in the repo sets or consumes `CRM_ENV=staging`. Fix: move `staging` out of the accelerated case in both switches — `staging` gets production durations and production overdue-day math; `accelerated` keeps the hour-scale behavior for future compressed-time environments. Ships as a separate small PR after PR 1; the staging box needs no `.env` change, just a redeploy + reseed. Rationale: under prod durations, overdue states are provided by the seeded world's time-shape (PR 1's backdated-creation cohort), not by compressed clocks — and a judge cannot be calibrated to a world where rendered dates and computed durations disagree by three orders of magnitude.

### F3 — The one "awaiting reply" row renders as "Untitled task" with a dead Todoist link

`SeedPendingFollowUp` (`replay/todoist.go:231-247`) writes `metadata = {due_date}` only; production's `FollowUpManager.applyCreate` (`followup_manager.go:525-532`) atomically writes `content`, `marker_json`, `project_id`, `label_name`, `integration_instance_id`. The API DTO reads `metadata["content"]` and the frontend falls back to the literal string **"Untitled task"** (`tasks-section.tsx:16-17`) instead of production's "Follow up: [Name](link) (awaiting reply)". It also leaves `external_task_id=''` on a `managed` row — a state production cannot produce (finalize sets state + id together, `todoist_op_worker.go:239-272`), and the "Open in Todoist" affordance links to `todoist://task?id=` with nothing after `id=`. Measured: 1/1 followup_loop rows affected, every run. This is the sole evidence row for the CAD-029/036 "awaiting reply" behaviors — the exact row the judge inspects to confirm the feature exists. Highest fix priority per token of effort.

### F4 — Every message-sourced interaction is inbound; conversations are 100% one-sided

Measured: **48/48** message-source rows (email/telegram/gchat/messages, 12 each) are `inbound`; zero `outbound` or `mutual` for any message source, in any profile. Root: all four factories only build inbound payloads (`Generator.GmailMessage` "builds an inbound email" `sources.go:71-117`; `TelegramMessage` adapter hardcodes `Out: false` `replay/telegram.go:64`; same for GChat/iMessage) — while the real providers carry both directions (`gmail.go:875-878`). Combined with the ≥21-day spacing (`WithMessageAge`), no reply-bridging or promote-to-mutual ever fires. Judge-facing symptom: every conversation surface shows the user never once replying — reads as a broken compose/send pipeline, the same false-regression class as CAD-036. Also leaves the direction-inclusive dedup paths and the email promote-to-mutual branch structurally unexercised.

### F5 — `external_sync_state` is completely empty; Settings shows "never connected" over a world full of synced data

Measured: **0 rows** despite six sources' interaction histories. Root: the replay adapters construct in-memory `SyncState` structs and never persist them (`gmail.go:54-58`, `gcal.go:115`, `todoist.go:143-150`); the gchat adapter persists then deletes its row in the same call (`gchat.go:56-64`). The Settings sync UI, sync badge, and StalenessService all read this table. Fix: persist prod-shaped sync-state rows (source, account, cursor, last-sync timestamps) as a seed step or in the adapters.

### F6 — Notes coverage is 3% (5/167)

The core "capture context about people" surface is empty on 162 of 167 contact pages — likely to read as "feature unused/broken" to a judge touring contact detail. `runCatalogProfile` seeds the notepad bucket minimally by design. Fix is a knob, not a redesign: seed notepad notes on a realistic fraction of catalog contacts (prod's actual fraction is measurable and can calibrate it).

### F7 — cadence_due contact_task incoherences (real, but currently invisible to a UI judge)

Measured: all **150** todoist cadence_due rows carry a raw-UUID temp `external_task_id` forever (the fake client returns no `TempIDMap`, so the same-tick finalize never runs — in prod the temp id is a sub-second transient); **1** `dismissed` cadence_due row exists (a combination no production code path can write — dismissal routes to `handleSkipTrigger` for cadence_due); **2** `completed` cadence_due rows have no outbound/mutual interaction within a day (production completion always implies one, and the next reconcile tick deletes completed rows anyway). Mitigating fact (verified): **no frontend surface fetches `lifecycle=cadence_due` today** — contact detail queries only manual + managed followup_loop. Correctness debt for any future API-level judge pass or cadence-due UI; not urgent for the current UI-only judge.

### F8 — One stranded knowledge assertion (invisible today, visible when SP1 UI ships)

Measured: exactly **1** contact with a current-accepted `birthday` assertion whose `contact.birthday` cache is NULL — the predicted `profiles.go:558-564` write, which drives the real `AssertService.Assert` but through a bare service with no `KnowledgeCacheUpdater` wired (`replay/assertion.go:26-35`), a partial replay of the causal chain. Everything else in the knowledge family measured clean (person-node dual-write 0 violations both directions; cache↔assertion coherent for lives_in/how_met; the 5 open assertions on dead nodes are all on soft-deleted contacts — prod-accurate retention). Fix: have `ReplayAssertion` refresh the cache for cutover predicates, or route the birthday demo through the contact-create authority flip.

### F9 — Coverage absences (world is narrower than prod, not incoherent)

Zero interaction rows for `todoist`, `anarlog_sessions`, `phone_calls` in any profile; gcal multi-attendee fan-out never seeded (factory hardcodes 2 attendees — the multi-entity `source_ref` dedup P0 has zero seed coverage); contact_method mix is 168 email / 3 telegram / 3 phone with no other types; per-contact message spacing is a uniform 21 days (non-organic but harmless). File under "expand when a tour needs it."

## What measured clean (the architecture is right)

External-identity exact-match integrity (0/10 violations), matched external_contact liveness, contact_method value shapes, interaction `source_ref` formats, dedup uniqueness (interaction and contact_task, both directions), event-bus per-entity `source_id` fan-out keys, venue population per source, person-node dual-write, proposition-slot uniqueness, `last_outreach_at`/`last_response_at` interaction-existence (0 violations each — the settled per-source cohort is fully coherent). The replay-through-real-providers design works; the violations cluster exactly where the seed bypasses or partially drives a production path.

## Judge-calibration note (not a seed fix)

Contact soft-delete does NOT cascade in production (`service/contact.go:552-577` touches only contact + node); live contact_method/note rows pointing at soft-deleted contacts are prod-accurate (measured: 4 methods on deleted contacts). If the judge ever flags this, it is a true positive about prod behavior, not a seed artifact.

## Recommended fix packaging

1. **PR: cadence causality** (F1) — replace `WithOverdue`/`WithRecent` endpoint writes with cause-driven shapes (backdated `created_at` cohort + replayed-interaction cohort), and add a post-seed **coherence gate** to the profile coverage test asserting zero violations of the production-impossible invariants (F1's two, plus T1/T4/T5 from F7/F3 and the F8 cache check). The gate is what prevents instance four from being discovered by a confused judge.
2. **PR: follow-up row shape** (F3) — small; full prod metadata + synthetic alphanumeric external id in `SeedPendingFollowUp`.
3. **Decision: staging cadence scale** (F2) — environment design question, decide before or alongside PR 1 (the overdue cohort's shape depends on which durations staging runs).
4. **PR: outbound/mutual message variants** (F4) — factory + adapter work per source.
5. **PR: sync-state persistence + notes coverage** (F5, F6) — both small seeding additions.
6. **Backlog:** F7 (pair with any future cadence-due UI or API-level judging), F8 (pair with SP1 UI), F9 (expand per-tour).

## Appendix: measurement summary (staging, 2026-07-13, 167 live contacts)

| Check | Violations / population |
|---|---|
| last_contacted < created_at | 111 / 114 |
| last_contacted > created_at without last_response_at | 0 / 3 |
| non-creation last_contacted, no last_interaction_at | 95 / 114 |
| non-creation last_contacted, no matching inbound/mutual interaction | 95 / 114 |
| last_outreach_at without matching outbound/mutual interaction | 0 / 7 |
| last_response_at without matching inbound/mutual interaction | 0 / 19 |
| contact_by set while cadence unset | 0 / 167 |
| contact_by ≠ last_contacted + prod-scale cadence | 149 / 149 (see F2 — env, not seed) |
| live contact without live person node (and inverse) | 0 / 167 both |
| accepted cutover assertion without cache column | 1 (birthday) |
| open assertions on dead nodes | 5, all on soft-deleted contacts (prod-accurate) |
| dismissed cadence_due (production-impossible) | 1 / 150 |
| completed cadence_due without nearby interaction | 2 |
| todoist rows stuck on raw-UUID temp id | 150 / 150 |
| managed followup_loop with empty external_task_id | 1 / 1 |
| followup_loop missing required metadata keys | 1 / 1 |
| message sources with zero outbound/mutual | 4 / 4 sources (48 rows all inbound) |
| interaction sources with zero rows | todoist, anarlog_sessions, phone_calls |
| gcal multi-attendee fan-out events | 0 seeded |
| external_sync_state rows | 0 (six sources active) |
| contacts with notepad notes | 5 / 167 (3%) |
| exact-matched identity without contact_method | 0 / 10 |
| all uniqueness/dedup/source_ref-shape checks | clean |
