# eventbus

Event bus wiring for the worker-queue migration (see issue #180 and
`.ai/spec/event-bus-foundation.md`).

Planned contents (per spec §3.3 and §3.4):

- `client.go` — `river.Client` construction, worker registration, lifecycle.
- Worker wrappers that fetch an event by ID and invoke a consumer's
  `HandleEvent(ctx, tx, env)` method.

The typed event kinds and `Bus.Publish` helper live in a separate package
`backend/internal/events/` — see spec §3.2.

Empty in PR 1 of the sequence (only the River client + a no-op worker are
wired; both live in `backend/cmd/crm-api/`). First code arrives in PR 2.
