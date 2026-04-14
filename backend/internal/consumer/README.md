# consumer

Consumer services that subscribe to events and perform domain writes
(see issue #180 and `.ai/spec/event-bus-foundation.md`).

Planned contents (per spec §3.4):

- `interaction_recorder.go` — writes `interaction` rows (§3.4.1).
- `cadence_updater.go` — writes the four cadence columns on `contact` (§3.4.2).
- `followup_manager.go` — creates/closes Todoist follow-up tasks (§3.4.3).
- `rematch_dispatcher.go` — re-runs identity matching on
  `contact_methods.added` (§3.4.4).

Empty in PR 1 of the sequence. First code arrives in PR 5.
