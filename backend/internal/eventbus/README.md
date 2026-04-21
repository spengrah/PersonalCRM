# eventbus

Event bus wiring for the worker-queue migration (see issue #180 and
`.ai/spec/event-bus-foundation.md`).

## Package contents

- `client.go` — `river.Client` construction, worker registration, lifecycle.
- Worker wrappers that fetch an event by ID and invoke a consumer's
  `HandleEvent(ctx, tx, env)` method.

The typed event kinds and `Bus.Publish` helper live in a separate package
`backend/internal/events/` — see spec §3.2.

## `EVENT_BUS_CADENCE_MODE` — operator reference

Post-cutover (PR 8), the `CadenceUpdater` consumer is the **sole writer** of
`contact.last_contacted`, `contact.last_outreach_at`,
`contact.last_response_at`, and `contact.contact_by`. The legacy direct-path
cadence SQL in `ContactService.applyInteractionEffectsFromRow` has been
removed. This changes what each mode value means — read carefully before
flipping the flag.

| Mode value | Behavior | When to use |
|---|---|---|
| `cutover` | Default. `CadenceUpdater` writes cadence columns for every `interaction.recorded` event (inline on the recorder path; durable no-op on queued retries). | Always, in production. |
| `shadow` | Legacy PR 7 mode. After cutover there is no direct path to observe, so shadow mode no longer observes anything. Parseable for migration compatibility only. | Never, post-cutover. |
| `off` | **Dangerous.** Disables the sole writer entirely. No cadence columns will be updated by any path. Startup refuses to boot unless the unsafe override is explicitly set. | Emergency ops only. |

### Rollback is `git revert`, not a flag flip

The PR 8 cutover deletes the legacy direct path. Setting
`EVENT_BUS_CADENCE_MODE=off` **does not** re-enable direct-path writes — it
just silences the sole remaining writer. To roll back PR 8 you must
`git revert` the cutover commit(s) and redeploy the prior binary.

### Emergency escape hatch

Startup gates `EVENT_BUS_CADENCE_MODE=off` behind an explicit opt-in:

```bash
EVENT_BUS_CADENCE_MODE=off
EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF=true
```

Only use this combination when you have decided, with eyes open, that
cadence columns must stop updating temporarily (for example, to isolate a
runaway consumer bug while the `git revert` is being prepared).

While the override is honored, `config.Load` emits a WARN log on every
config load with these fields (acceptance criterion 4):

- `event=cadence_mode_off_unsafe_override_active`
- `cadence_mode=off`
- `unsafe_allow_off=true`
- `recovery="unset EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF to restart in safe mode; git revert PR 8 if you need the direct path back"`

**What breaks while `mode=off` is active:**

- Every new `interaction.recorded` event is dropped by the consumer. No
  new writes to `last_contacted`, `last_outreach_at`, `last_response_at`,
  or `contact_by`.
- `MergeContacts`, `ExtendInteraction`, `PromoteInteractionToMutual`, and
  the link/import cadence-override path all route through `CadenceUpdater`,
  so they also stop updating cadence columns while off.
- The underlying `interaction` rows, follow-up tasks, and Todoist
  reconciliation continue to work — only cadence state is frozen.
- Event durability survives across mode toggles: events are still
  published and durably stored. Flipping back to `cutover` does **not**
  retroactively process the events that were dropped while off; use the
  standard replay paths if you need to backfill.

### Test harnesses

`config.TestConfig()` sets `CadenceMode=off` AND `UnsafeAllowOffMode=true`
by default so unit/integration tests can exercise off-mode branches without
tripping the production startup gate. Tests that need `cutover` override
`cfg.EventBus.CadenceMode` explicitly.

## Durable cadence claims

The `event_consumer_claim` table (migration 040) enforces exactly-once
semantics for inline + queued delivery of the same `interaction.recorded`
event. The inline path from `InteractionRecorder` claims
`(event_id, consumer='cadence_updater')` before mutating cadence; a
later queued delivery of the same event finds the existing claim row
and returns a no-op.

## `EVENT_BUS_FOLLOWUP_MODE` — operator reference

In shadow mode, the `FollowUpManager` consumer observes what it *would*
do for each `interaction.recorded` event and writes a row to
`event_shadow_followup_observation`. The direct path
(`service/followup.go` + `ContactService.deriveFollowUpClosure`) remains
the authoritative writer of `contact_task` follow-up rows. No Todoist
API calls are made by the consumer in shadow mode.

| Mode value | Behavior | When to use |
|---|---|---|
| `shadow` | Default. Consumer observes + writes shadow rows; direct path stays authoritative. | Default during the shadow-mode phase. |
| `off` | Consumer is disabled entirely. Direct path remains authoritative. Rollback-safe posture. | Emergency rollback of the shadow consumer. |
| `cutover` | Would run the consumer as sole writer (two-step `contact_task` insert + Todoist task create/close). Not implemented in shadow-mode builds; returns an error if invoked. | Never until the cutover ships. |

Unlike `EVENT_BUS_CADENCE_MODE`, there is no unsafe-override gate on
`off` — because the direct path is still the authoritative writer,
turning the consumer off is safe and harmless.

### The three skip guards

Outbound `interaction.recorded` events run through three sequential
guards before the consumer would create a new follow-up:

1. **Backdated outbound (non-manual only).** If
   `now - occurred_at > watchdog_days(cadence)`, skip with
   `skip_reason='backdated'`. Manual-source interactions bypass this
   guard — the user is intentionally recording a stale outbound.
2. **Out-of-order delivery.** If any later inbound/mutual interaction
   already exists for the contact, skip with
   `skip_reason='out_of_order'`.
3. **Duplicate pending follow-up.** If a follow-up in state `managed`
   or `pending_remote_create` already exists, record a `refresh`
   action instead of creating a new row — matches the direct path's
   behavior.

All three guards fire only on the consumer side. The direct path
creates a follow-up regardless, so during bake the shadow-divergence
query surfaces guard-1 and guard-2 hits as **expected** divergences
(documented in the post-bake report, not treated as acceptance
failures).

### Post-bake divergence query

```sql
WITH direct_obs AS (
    SELECT event_id, action AS direct_action, direct_contact_task_id
    FROM event_shadow_followup_observation
    WHERE writer = 'direct'
      AND observed_at >= NOW() - INTERVAL '48 hours'
),
consumer_obs AS (
    SELECT event_id, action AS consumer_action,
           skip_reason AS consumer_skip_reason,
           consumer_called_todoist
    FROM event_shadow_followup_observation
    WHERE writer = 'consumer'
      AND observed_at >= NOW() - INTERVAL '48 hours'
)
SELECT
    COALESCE(d.direct_action, 'missing') AS direct,
    COALESCE(c.consumer_action, 'missing') AS consumer,
    c.consumer_skip_reason,
    COUNT(*) AS event_count,
    CASE
        WHEN d.direct_action = 'create' AND c.consumer_action = 'skip'
             AND c.consumer_skip_reason = 'backdated'
             THEN 'expected_guard1_backdated'
        WHEN d.direct_action = 'create' AND c.consumer_action = 'skip'
             AND c.consumer_skip_reason = 'out_of_order'
             THEN 'expected_guard2_out_of_order'
        WHEN d.direct_action IS NULL AND c.consumer_action IS NOT NULL
             THEN 'expected_external_or_closure_gap'
        WHEN d.direct_action = c.consumer_action THEN 'agreement'
        ELSE 'unexpected'
    END AS class,
    BOOL_OR(c.consumer_called_todoist) AS any_consumer_todoist_calls
FROM direct_obs d
FULL OUTER JOIN consumer_obs c USING (event_id)
GROUP BY 1, 2, 3
ORDER BY class, event_count DESC;
```

Shadow acceptance requires:
- `SELECT COUNT(*) FROM event_shadow_followup_observation WHERE writer='consumer' AND consumer_called_todoist = true;` → **0**
- The `unexpected` class in the report above → **0** event_count after
  filtering the documented expected classes
