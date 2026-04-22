# consumer

Consumer services that subscribe to events and perform domain writes
(see issue #180 and `.ai/spec/event-bus-foundation.md`).

## Consumers

| File | Role | Mode |
|---|---|---|
| `interaction_recorder.go` | Sole writer of `interaction` rows (§3.4.1). | Cutover. |
| `cadence_updater.go` | Sole writer of `contact.last_contacted`, `contact.last_outreach_at`, `contact.last_response_at`, `contact.contact_by` (§3.4.2). | Cutover. |
| `followup_manager.go` | Sole writer of `contact_task.kind='follow_up'` lifecycle (§3.4.3). | Cutover. |
| `rematch_dispatcher.go` | Re-runs identity matching on `contact_methods.added` with per-contact serialization (§3.4.4). | Cutover. |
| `todoist_followup_workers.go` | River workers for follow-up Todoist create / close / refresh jobs. | Cutover. |

Each consumer has an `EVENT_BUS_*_MODE` config flag that accepts
`cutover` (default) or `off` (emergency kill switch, requires the
paired `EVENT_BUS_*_UNSAFE_ALLOW_OFF=true` safety gate).

## FollowUpManager — cutover overview

`FollowUpManager.HandleEvent` runs on `interaction.recorded` envelopes.
In cutover mode it is the sole writer of the `contact_task.kind='follow_up'`
lifecycle: three-guard skip logic, two-step crash-safe create
(`pending_remote_create` → `managed` once the Todoist `item_add`
succeeds), refresh with post-commit Todoist call + river-retry
fallback, and complete with a river-retried `TodoistFollowUpCloseJob`.

### Three skip guards (outbound events)

1. **No cadence:** contact has no cadence string → no follow-up.
2. **Backdated:** non-manual outbound whose `occurred_at` is older than
   the cadence's watchdog window. Manual source bypasses this guard.
3. **Out-of-order:** a later inbound/mutual interaction is already on
   record, so creating a follow-up now would be stale.

### Test-only DecisionObserver hook

`FollowUpManager` exposes a `SetDecisionObserver(DecisionObserver)`
method that emits a `Decision` struct at each terminal branch (action,
skip reason, would-be deadline, idempotency key, contact_task id).
Production wiring leaves the observer nil — the hot path pays only a
nil check per branch. Unit tests install a closure that appends to a
slice to assert on classification coverage.

See `backend/internal/eventbus/README.md` for the
`EVENT_BUS_FOLLOWUP_MODE` operator reference and the `.ai/guides/architecture.md`
"Event Bus and Consumers" section for the hybrid sync/async contract.
