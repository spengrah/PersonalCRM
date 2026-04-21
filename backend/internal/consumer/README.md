# consumer

Consumer services that subscribe to events and perform domain writes
(see issue #180 and `.ai/spec/event-bus-foundation.md`).

## Landed consumers

| File | Role | Mode |
|---|---|---|
| `interaction_recorder.go` | Sole writer of `interaction` rows (§3.4.1). | Cutover. |
| `cadence_updater.go` | Sole writer of `contact.last_contacted`, `contact.last_outreach_at`, `contact.last_response_at`, `contact.contact_by` (§3.4.2). | Cutover. |
| `followup_manager.go` | Observes follow-up create/refresh/complete/skip decisions alongside the authoritative direct path (§3.4.3). | Shadow. |
| `todoist_followup_workers.go` | River workers for follow-up Todoist create/close jobs. Registered so river knows the kinds; worker bodies refuse to execute until cutover ships. | Inert in shadow. |

## Pending

- `rematch_dispatcher.go` — re-runs identity matching on
  `contact_methods.added` (§3.4.4).

## FollowUpManager — shadow-mode overview

`FollowUpManager.HandleEvent` runs on `interaction.recorded` envelopes.
In shadow mode it evaluates the three spec guards for outbound events
and the pending-follow-up lookup for inbound/mutual events, then
records the decision (create / refresh / complete / skip) to
`event_shadow_followup_observation`. It never writes `contact_task`
rows or calls Todoist — the direct path (`service/followup.go` via
`ContactService.deriveFollowUpClosure`) remains the authoritative
writer.

The direct path's post-commit closure also writes a `writer='direct'`
observation row, keyed on the same `interaction.recorded` event id as
the consumer's `writer='consumer'` row. The post-bake divergence
report FULL OUTER JOINs them and flags anything outside the
documented expected-divergence classes.

See `backend/internal/eventbus/README.md` for the
`EVENT_BUS_FOLLOWUP_MODE` operator reference and the post-bake query.
