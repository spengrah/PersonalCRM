# Anarlog CLI source

Restores the Mac daemon's Anarlog sync by reading sessions and the people in them through the Anarlog CLI, after Anarlog moved its storage from a file tree into SQLite. Issue context: [#834](https://github.com/spengrah/PersonalCRM/issues/834) (raw transcripts, which this spec does not deliver) and the parent specs [`mac-daemon.md`](./mac-daemon.md) and [`mac-daemon-phase-2-anarlog-matching.md`](./mac-daemon-phase-2-anarlog-matching.md).

Engine bindings: `.ai/log/plan/anarlog-cli-source-bindings.json`

## Context & problem

On 2026-08-04 Anarlog (1.4.x, formerly Hyprnote) imported its file tree into its own SQLite database and stopped writing session and people files. Anarlog's own changes call the files "a leftover from the markdown era"; the file tree will not come back. New sessions get a directory holding only audio, which the daemon counts as missing metadata and never sends.

The daemon's two Anarlog sources, `anarlog_sessions` and `anarlog_humans`, still read that frozen file tree. The CRM's newest meeting note is from 2026-07-24. Every session and every new person recorded in Anarlog since then is missing from the CRM, and nothing raised an alarm for two months: each tick succeeded and found nothing new.

Anarlog ships a CLI (`anarlog`, installed with the app) that reads the new database read-only and emits JSON under a documented contract version (`schema_version: "1"`, with machine-readable errors). It exposes meetings (list, get, note, transcript, history, export) but no people command; people appear only as meeting participants. Anarlog documents no programmatic read path for people anywhere (CLI, MCP, cloud API, webhooks). Session IDs and person IDs are identical between the old file tree and the new database, so the CRM's existing meeting notes and Anarlog identities remain valid.

## What matters

In priority order, as confirmed by the user:

1. **Restore the Anarlog → CRM flow through the Anarlog CLI.** Sessions (title, time, summary, memo, participants) and the people who appear in them reach the CRM again, and the first run catches up everything missed since 2026-07-24. Existing meeting notes and Anarlog identities carry over.
2. **A break is never silent again.** A broken CLI, a changed CLI contract, or a missing operator identity raises one macOS notification that stays until fixed. A quiet break, where the CLI succeeds but nothing new arrives, also errs loudly by a cheap means.
3. **The CLI is the only read path.** Nothing reads Anarlog's database or file tree directly.
4. **The file-tree readers are deleted,** with the file watching, configuration and diagnostics that serve them.

## Intent & appetite (delegation charter)

**What this is for.** The Anarlog pipe is how meetings become interactions and evidence in the CRM, and it is the future substrate for LLM extraction (#379). It runs unattended in the background on the user's Mac, hourly or faster. The cost of failure has been demonstrated: two months of meetings silently missing. The user attends only when a notification appears. This arc also serves as a medium-sized test of the dev-workflow runner.

**Appetite.** Restoration only: bring back the behavior the file-tree readers had, on the new source, plus loud failure. The user explicitly declined expansion into anything else.
- A new session is first sent only once it has settled: three hours after its creation. Anarlog records no completion state the CLI exposes, so age stands in for "the meeting is over", and the delay gives the user time to tag participants before an orphan notification fires. After that, one hour from a change in Anarlog to the CRM reflecting it is the planning target. The user will accept more if the design needs it; a contract that relaxes the target records the relaxation and why. Slower beats fragmented: prefer one coarse, complete read over fine-grained triggers.
- One re-send of all history on the first run is acceptable, and so is the burst of orphan and conflict notifications it may raise. The re-send also rewrites every existing summary and memo without the YAML header the old files carried; that change is welcome.
- One sticky notification per broken condition, not repeated reminders. A condition retrying cannot fix (a missing CLI binary, a contract version other than `"1"`, a missing field the daemon uses, an unset operator person ID) notifies on the tick that observes it. A runtime failure (a non-zero exit, a missing database, a timeout) notifies only after failing on two consecutive ticks, so a single transient failure never notifies.
- The quiet-break guard must stay cheap. The user said "don't boil the ocean" for it.

**Rabbit holes.**
- Handling more than one live summary on a session. None has been observed; regenerating a summary replaces the old one. Treat as hypothetical until observed.
- Detecting that Anarlog merged or deleted a person. Anarlog has never deleted a person in the user's data. Treat as hypothetical. People are never deleted from the CRM by this source (see Hard constraints).
- Reaching people who never appear in a session, or people fields no CRM code reads (LinkedIn username, pinned, pin order, person memo, organization). The user ruled these out.
- A compatibility matrix across CLI versions, or a manifest describing the CLI contract. The runtime contract check and the throwaway real-CLI proof are the whole answer.
- Tests of the fixtures themselves (second-order tests). The user does not want second-order test suites; synthetic fixtures are a convention, not a gate.
- Anarlog's cloud source, webhooks (delivered only while the app is open, dropped otherwise), or its MCP server as the transport.
- Pi-side changes beyond what restoration strictly needs. The wire payloads keep their shape.

**Call-me-when triggers.** Bring these to the user instead of judging silently:
- A session carries more than one live generated summary (for example a default summary and a template output together).
- Anarlog merges or deletes a person the CRM already knows.
- The CLI lacks a field or behavior the work needs, beyond the operator's person ID. Reading Anarlog's database directly is not an available fallback.
- Restoration appears to require changing a wire payload's shape or a Pi-side table.

**User-reserved decisions.** Engine bindings; any write to prod data outside the normal ingest path; installing the daemon on the user's Mac and the first real sync (after this arc, done with the coordinator's help, not inside the arc); promotion to prod.

**Tradeoff rules.** Tradeoffs operate only inside the Hard constraints; restoration that cannot be achieved within them goes to the user. When priorities collide, restoration (1) outranks loudness (2), which outranks purity of the read path (3), which outranks deletion (4). Within loudness, a false alarm the user can dismiss beats a missed break. Between a simpler design and one that preserves byte-identical payloads for legacy sessions, choose simpler: the churn pass is accepted.

## Goals

- Sessions and their participants flow from Anarlog to the CRM through the Anarlog CLI, with the same payload shapes the file-tree readers produced.
- The first run catches up all sessions since the backfill floor that the CRM lacks, and updates existing ones.
- Loud, sticky, self-clearing notifications cover CLI breakage, contract changes, a missing operator identity, and a quiet source.
- The file-tree readers and everything that exists only to serve them are removed.

## Non-goals

- Raw transcript ingest. #834 stays open; the CLI's `meetings transcript` command is the likely path when it resumes.
- People who never appear as a session participant; people metadata no CRM code reads.
- Multiple summaries per meeting note on the wire.
- Carrying Anarlog's ProseMirror document format on the wire. The wire carries markdown.
- The Pi-side quarantine for rejected daemon events ([#460](https://github.com/spengrah/PersonalCRM/issues/460)) and the Go↔Swift wire contract ([#343](https://github.com/spengrah/PersonalCRM/issues/343)).
- Installing the rebuilt daemon on the user's Mac.

## Relation to existing & planned work

- **Builds on** the Anarlog matching design ([`mac-daemon-phase-2-anarlog-matching.md`](./mac-daemon-phase-2-anarlog-matching.md)): linkage, orphan and conflict handling, and the meeting-note re-sync semantics (ING-032, ING-033, NTS-022, NTS-024) are unchanged and must keep working on CLI-sourced payloads. The orphan and conflict notification surface (MAC-041, MAC-042) is the notification system the new failure notifications reuse.
- **Changes** MAC-040, whose then-item says a filesystem change under the sessions directory triggers a sessions tick. That trigger goes away with the file tree. MAC-043's doctor checks of the Anarlog directories change with it. Apply the ID lifecycle in `spec/README.md` (extend in place versus retire and mint) when the implementation lands.
- **Adjacent:** MAC-036's Pi-side push-staleness watchdog did not catch this outage, because the daemon kept reporting successful pushes while finding nothing new. The quiet-break guard here is daemon-side by the user's ruling; the watchdog is not changed.
- **Partially overlaps #343:** #343 lists the daemon's session-metadata lookup (which re-reads session files to label notifications) as a deletion target. This arc must replace that lookup's file reads anyway, through the CLI. #343 itself stays open and out of scope.
- **Defers #834:** transcripts are out; the CLI's transcript command is noted there as the path forward.
- **Feeds #379:** restores the meeting-note content the interactions timeline shows as evidence (IXN-005) and that SP3 extractors will read.

## Prior art & external constraints

- **Anarlog CLI contract** ([docs.anarlog.so/reference/cli.md](https://docs.anarlog.so/reference/cli.md)): `schema_version` is "the CLI JSON contract version", successful `--json` output is `{schema_version, command, data, pagination?}`, and errors are machine-readable JSON. No compatibility policy states what triggers a version bump. The CLI is intended for agent and program use ([docs.anarlog.so/agents/cli.md](https://docs.anarlog.so/agents/cli.md)).
- **Observed CLI behavior (1.4.27):** listing returns live sessions only, newest first by `created_at`, in pages of up to 200, with `next_offset` null on the last page. `meetings get` returns title, `created_at`, a note (the memo) and a `summaries` list as markdown for both legacy markdown-bodied and newer ProseMirror-bodied documents, and participants with person ID, display name, email, job title and organization. `updated_at` does not move when a summary is regenerated or participants change. Errors: `not_found` (exit 2) for an unknown, malformed or deleted ID; `database_not_found` (exit 3) for a missing database. No running app or sign-in is needed for local reads. Fetching every session takes seconds.
- **Summaries and templates:** a default summary is a `summary` document; a summary produced by a user-chosen template is a `template_output` document that takes the summary's place. The CLI lists both under `summaries`.
- **Operator identity:** the CLI does not say which participant is the user. Legacy sessions mark the user with the all-zero ID; sessions since the migration include the user as an ordinary person who participates in every session.
- **Anarlog guidance:** the docs warn against editing the app database and document no schema. The CLI's bundled MCP server instructs callers never to access SQLite directly. Anarlog is MIT-licensed open source ([github.com/fastrepl/anarlog](https://github.com/fastrepl/anarlog)).
- **Rejected:** reading the database directly (couples the daemon to an undocumented, fast-moving internal schema and to rendering ProseMirror); webhooks (local-only, dropped while the app is closed, no people events); carrying raw ProseMirror on the wire (moves rendering to the Pi and spreads a source-specific format into the backend).

## Hard constraints

- Every read of Anarlog data goes through the `anarlog` CLI's `--json` output. No direct reads of Anarlog's database or file tree.
- The Anarlog people source never deletes a person from the CRM: not when their last session is deleted, not when they are removed from a session, not when they stop appearing in the CLI. The CRM keeps every identity it has.
- Each Anarlog source's existing enable switch keeps its meaning: disabling sessions does not stop people syncing, and disabling people does not stop sessions syncing.
- The wire payloads (`meeting_note.recorded`, `meeting_note.deleted`, and the Anarlog people external-contact payload) keep their current shape and version; the CRM-side ingest contract is unchanged.
- The operator's Anarlog person ID is configured once in the daemon's config. The daemon never infers it.
- Test fixtures are synthetic. Nothing copied from Anarlog's database or real CLI output is committed, in fixtures, logs, PR bodies or spec files (repo privacy rule).
- The backfill floor (sessions created before 2026-01-01 are never sent) is preserved.

## Architectural direction

- **Constraint:** the Anarlog CLI is the source abstraction, chosen by the user so a future Anarlog storage change is Anarlog's problem to absorb in its CLI.
- **Constraint:** standing tests run against a fake CLI at the process boundary, covering success and each failure mode the real CLI produces. During the build, a throwaway test runs the real CLI on the user's Mac against the real database and compares only the *shape* of its output (keys, types, error codes, exit codes) with the fake's. The result goes in the PR body; the test is deleted and never committed. The fake's reference is the real CLI's output, not Anarlog's MCP schemas, which omit the CLI's envelope.
- **Constraint:** every CLI response passes through one decode step that requires `schema_version == "1"` and every field the daemon uses, ignoring unknown additional fields.
- **Constraint:** a session is first sent only once it is at least three hours old (the settle time). Anarlog's session status and end time carry no completion signal, and gating on the presence of a summary would drop sessions that never receive one.
- **Constraint:** the quiet-break guard is one condition evaluated every tick: no session in the CLI's listing was created within the last 14 days, which includes an empty listing. The notification is raised while the condition holds and cleared once it no longer holds.
- **Constraint:** people are derived from the participants of every eligible session (on or after the backfill floor and past the settle time), not only the sessions re-sent on a tick, so a change to a person is sent on its own.
- **Leaning:** people continue to flow as the existing `anarlog_humans` source's payloads.

## Success criteria

- After the daemon runs with the change, every live Anarlog session created on or after 2026-01-01 and at least three hours old exists as a meeting note in the CRM, with its summary and memo matching the CLI's markdown and its participants resolved as before.
- Every person who participates in such a session, other than the operator, exists as an Anarlog identity in the CRM, and no identity the CRM held before is deleted.
- A missing CLI binary or a contract version other than `"1"` produces exactly one macOS notification on the tick that observes it, and restoring the CLI clears the notification on the next clean tick.
- No code path in the daemon reads the Anarlog file tree; the file-tree readers, their watcher, and their configuration are gone.
- The PR body records the throwaway real-CLI shape proof: what was compared, the command run, and its result.

## Desired behavior sketch

The IDs below are minted as `proposed` behaviors in `spec/mac-host.yaml`. All are `surface: none`: the daemon's Swift tests prove them, and the coverage scanner does not read Swift.

- **MAC-047** Sessions are read through the Anarlog CLI: each tick lists every live session and sends those at or after the floor and at least three hours old, with title, time, generated summary (default or templated), memo and participants other than the operator. A session younger than three hours is not sent. A settled session, and any later change to it, reaches the CRM within the latency target.
- **MAC-048** A session is re-sent when anything its payload derives from changes, detected from the CLI's record rather than Anarlog's `updated_at`.
- **MAC-049** A session that disappears from the CLI's full listing is deleted in the CRM.
- **MAC-050** People are the participants of every eligible session, excluding the operator, sent as Anarlog identities with ID, name, email and job title; a change to a person is sent on its own; people who appear in no eligible session are not sent; no person is ever deleted by this source.
- **MAC-051** The operator's Anarlog person ID is set once in the daemon config; while it is unset, the Anarlog sources send nothing and raise the source-broken notification.
- **MAC-052** A broken CLI raises one sticky notification. A missing binary, a contract version other than `"1"`, or a missing field the daemon uses notifies on the tick that observes it; a runtime failure notifies after two consecutive failed ticks. A single transient failure does not notify, unknown additional fields are ignored, and the notification clears on the next clean tick.
- **MAC-053** While no session in the CLI's listing was created within the last 14 days (including an empty listing), one notification says no new Anarlog sessions have arrived; it clears once a session created within the last 14 days appears.
- **MAC-054** The daemon never reads Anarlog's database or file tree directly (invariant).
- **MAC-055** The Anarlog people source never deletes a person from the CRM (invariant).

## Assumptions & deferred questions

- **Assumed:** the `anarlog` CLI is installed with Anarlog and updated with it, as observed for 1.4.27. How the daemon, running as a background agent with a minimal environment, locates the binary is left to planning; failing to locate it is a MAC-052 condition.
- **Assumed:** the operator's person ID is stable across Anarlog updates. The user can read it from Anarlog; how the setup step surfaces it is left to planning.
- **Assumed:** the zero-ID sentinel that marks the user in legacy sessions continues to be excluded as before.
- **Left to planning:** the tick cadence within the one-hour latency appetite; how change detection hashes the CLI record; how deletion reuses the existing known-ids reconciliation; the arc's PR decomposition; which existing behaviors (MAC-040, MAC-043) are extended in place versus retired.
