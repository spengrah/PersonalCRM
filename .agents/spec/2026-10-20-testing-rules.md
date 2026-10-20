# Testing rules that serve the suite's goals

## Context & problem

`.agents/rules/testing.md` is the repository's testing rule file: agents load it on demand (AGENTS.md "Context Discovery"), and `code-review.md`, `feature-development.md` and `backend-troubleshooting.md` link into it. A read-only audit on 2026-10-20, of every rule in it against the tree, found that about a third of it is stale, ignored or contradicted by enforcement that already exists:

- Rules the suite ignores. "Heavy replay tests" that drain River must call `RequireLongTests` and move to the slow suite, yet 14 of the 23 synthetic-harness test files run in the fast lane. Every harness starts a River client, so "a test that starts a worker uses a per-test clone" covers all 23, yet 17 share a database. Database-backed test files "carry the `integration_testdb` build tag": 72 of 197 do, and the working gate is `testsupport.RequireDatabase`, which a static test enforces.
- Rules that contradict lint or code. An E2E wait snippet uses a literal timeout that ESLint rejects. "UI-created data → serial mode" describes isolation that serial mode does not give, while the suite isolates by `testApi.prefix`. "Delete by a fixed account ID before creating" collides under `t.Parallel()`, which another section requires. A client-factory pattern names a method that no longer exists and teaches a test-only branch in production code.
- Stale facts: a test directory that does not exist, E2E described as pre-deploy when it gates PRs, a modal component that was removed, an `@smoke` tag nothing consumes, and a lane list missing the integration target CI actually runs.
- Two writers of one fact. `make test-unit` hand-lists packages and skips 26 that CI's unit lane (`go test -short ./...`) runs, so a local run can pass while CI fails. The file's opening section duplicates `core.md`'s Testing Requirements.
- How-to snippets (a TestAPI method table, a date-formatting snippet, a Vitest fetch-mock pattern, imports-page gotchas) that restate helper source and JSDoc and have already drifted from it.

The trigger was PR #961: its approval gate flagged two rules, slow-lane routing and per-test clones. Following the routing rule would have moved the only tests proving two spec behaviors out of merge gating, because the slow lane runs nightly and no required check runs it. The toolkit's own pattern doc already says the opposite: route a test there only when detecting its regression a day late is acceptable.

## What matters

1. Proof: every behavior the product promises has a test that fails when it breaks, cited to its spec ID.
2. Gating: regressions are caught before merge.
3. Fidelity: the repository's own code runs against real PostgreSQL; fakes stand in only for what cannot run, and for injected failures.
4. Independence: tests are parallel-safe and deterministic.
5. Speed: the gating lanes stay cheap enough to run on every PR.
6. One way: each kind of test plumbing has one home, so an agent copying the nearest example copies the right one.

## Intent & appetite (delegation charter)

The artifact is the guidance agents follow when they write or review tests, read on every test-writing task and by every approval gate that judges tests. Its failure cost shows up in two ways: agents follow a wrong rule and build the wrong test (or move a test out of gating), or reviewers enforce a stale rule and block correct work. Both have happened.

The user's standing taste governs this work: a rule the codebase ignores is worse than no rule, so each rule is either enforced mechanically or rewritten to match practice; structure beats vigilance, so an invariant belongs in a type, a lint rule, a static test or a single source rather than in prose; a fact has one writer.

Appetite: one PR of guidance changes plus the small mechanical fixes that make a rule true (for example, one source for the unit-lane package list). Worth it: deleting, rewriting and relocating rules; pointing prose at the code or check that owns a fact. Not worth it: new lint rules or static tests for a rule whose violation is cheap or rare; rewriting existing tests to match a rule (existing tests convert when a change already rewrites them); a migration of the test suite.

Rabbit holes:
- Writing a new guard for every rule that survives. A rule that needs judgment stays with review.
- Re-documenting helpers in the rule file. Their source and JSDoc are their documentation.
- Auditing the pattern docs line by line. The passages that contradict a surviving rule are in scope and get fixed in the same change (the slow-test routing opening in `synthetic-seed-toolkit.md`, the River-worker trigger in `test-parallelism.md`, the mac-host lock holder list in `e2e-parallelism.md`); the rest of each doc is not.
- Converting existing tests. The rules govern new and substantially rewritten tests.

Call-me-when triggers:
- A rule's removal would leave a behavior without a merge-gating test.
- Making the synthetic harness open its own clone costs materially more than about a second per harness test, or breaks a harness caller that needs a shared database.
- Cost alone would move a behavior's last proving test out of a merge-gating lane.
- A proposed mechanical check would cost more than the failures it prevents.

User-reserved decisions:
- Any change to which CI checks are required.

The user ruled on 2026-10-20 (decision records `20261020T165735Z-ee581f301ea0`, `20261020T165736Z-70833400d345`, `20261020T165737Z-329f92b897dd`): a behavior's proving test runs in a merge-gating lane; the synthetic harness always opens its own database clone; `.agents/rules/testing.md` is deleted and its surviving judgment rules move into `core.md`.

## Goals / Non-Goals
Goals:
- `.agents/rules/testing.md` is deleted. Its surviving judgment rules live in `core.md`'s Testing Requirements, each tied to one of the six goals and naming its enforcement: a lint rule, a static test, a CI check, a single source in code, or reviewer judgment. How-to lives in the pattern docs, helper source and exemplar tests. Every incoming link points at the new home.
- The rules govern new and substantially rewritten tests. Existing tests convert only when a change already rewrites them.
- Each fact the guidance states has one home; other documents point at it.
- Isolation of synthetic-harness tests is structural: the harness always opens its own database clone, so no rule asks a harness test to request one.
- `make test-unit` is the one definition of the backend unit lane, and CI calls it.

Non-Goals:
- No conversion of existing tests to the new rules.
- No change to which CI checks are required.
- No new testing framework, lane, or lane-aware coverage scanner.

## Relation to existing & planned work

- `.agents/rules/core.md` "Testing Requirements" and "Code Quality" (second-order verification, proportionality) already hold the cross-cutting rules; this change must not duplicate them.
- `.agents/rules/code-review.md` points at testing.md for the fake rule and proportional verification.
- Bosun's review text ("Criteria and tests", "Scratch and kept tests") defines test level and the keep bar for every dispatched build and gate; the repository's rules should not restate or contradict it.
- `spec/README.md` owns test → behavior citations, enforced by `make spec-coverage` and `make spec-lint` in pre-push and CI.
- Pattern docs `.agents/patterns/synthetic-seed-toolkit.md`, `test-parallelism.md` and `e2e-parallelism.md` hold the how-to for the toolkit, backend parallelism and E2E parallelism.
- PR #961 (Gmail calendar-message tests on the synthetic toolkit) rewrote the lane-placement line in `testing.md` and the slow-test routing opening in `synthetic-seed-toolkit.md` to the ruled lane rule; this change carries that rule into `core.md`.
- Incoming links to `testing.md`: AGENTS.md (Context Discovery and Session Hints), `code-review.md`, `feature-development.md`, `backend-troubleshooting.md`.

## Hard constraints

- No PII in any rule or example (AGENTS.md Privacy).
- Required CI checks stay as they are.
- `core.md`'s absolute rules are unchanged.

## Architectural direction
- **constraint (user ruling, 2026-10-20):** delete `.agents/rules/testing.md`; its surviving judgment rules move into `core.md`, which every agent loads.
- **constraint (user ruling, 2026-10-20):** the synthetic harness always opens its own per-test database clone. Every harness starts a River client, and River's leader maintenance moves retryable and scheduled jobs to available across all queues (the pinned River's `JobSchedule` has no queue filter), so a private fetch queue does not isolate a harness on a shared database.
- **constraint (user ruling, 2026-10-20):** the test that proves a spec behavior runs in a merge-gating lane. The nightly lane holds only extra coverage whose measured cost outweighs catching its regression a day late; cost alone never moves a behavior's last proof there, and a second-order test that guards against vacuous passes gates a merge as `core.md` requires. Reviewer judgment enforces it: `make spec-coverage` records no lane.
- **leaning:** the surviving rules are (a) build state with the synthetic toolkit when a test needs reusable domain state, extending the toolkit when it cannot express that state; small repository fixtures and same-package tests of unexported logic stay as they are (the harness cannot be imported from packages it depends on); (b) what to fake, including asserting the outcome of an injected failure; (c) isolation: namespace scoping on a shared database by default, a per-test clone when correctness depends on database-wide state (database-wide counts or deletes, fixed IDs, singletons, a started River client), and the harness's own clone covering harness tests; (d) the lane rule above; (e) parallel by default; (f) E2E isolation by `testApi.prefix` and declared seeds; serial mode for ordering within one file when sibling tests change shared state the file reads, and the global lock for a singleton shared across files; (g) spec citations, by pointer to `spec/README.md`; (h) no copied unexported logic in tests.
- **leaning:** CI's unit step calls `make test-unit`, which keeps short mode, the environment the step sets and the separate WhatsApp race step, so package selection and execution have one source.

## Success criteria
- A one-time audit, recorded in the PR body, lists every rule `testing.md` held and where it went: into `core.md`, to a pattern doc or helper, to an existing check, or deleted with its reason.
- A walkthrough, recorded in the PR body: an author writing a new integration test and a reviewer judging it, following only `core.md` and the links it gives, reach a merge-gated, parallel-safe test, and neither meets a rule the suite contradicts.
- No document links to `testing.md`.
- `make test-unit` and CI's unit step select and run the same packages, shown by the rendered commands and one run of the target.
- A synthetic-harness test runs on its own clone without asking for one.

## Desired behavior sketch

An agent opens the testing guidance and reads, in a page or two, what it must do and why, with each rule pointing at the check or the code that owns it. When it needs how-to, it follows a pointer to a helper or an exemplar test. A reviewer judging a test reads the same rules and finds none it must discharge as stale.

## Assumptions & deferred questions
- Assumed: rules that must reach every test-writing agent belong in `core.md`, which every agent loads; AGENTS.md loads other guidance only as needed.
- Deferred: a lane-aware `make spec-coverage` that counts only citations from merge-gating lanes, which would enforce the lane rule mechanically. Reviewer judgment enforces it until the user expands scope.

## Engines

```json engines
{
  "authorFamily": "claude",
  "arcPlanner": "claude-opus-5-5:high",
  "arcReview": "gpt-6.1-sol:high"
}
```

The user ruled on 2026-10-20, for the preceding Gmail work, that Opus implements and gpt-6.1-sol reviews; this draft carries the same choice until the user rules otherwise.
