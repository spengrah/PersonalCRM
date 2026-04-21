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
