# Personal CRM verification map

This directory is the maintained source for how to drive Personal CRM's web UI to verify its behavior. Read this index before driving the app, then use the matching feature file as the recipe. `../SKILL.md` covers launch, doctor, the `lib/pw.mjs` session, evidence and cleanup.

Intended behavior lives in `spec/*.yaml` (see `spec/README.md`), keyed by behavior IDs such as `CON-055`. The feature files cite those IDs and never restate what they require: a restated outcome goes stale without warning when the spec changes. Judge every result against the cited behavior's `then` items and waivers.

## Baseline preconditions

- `bin/launch` started this checkout's instance and `bin/doctor` exits 0.
- The database holds the synthetic `standard` world: every seeded name starts with `synth-standard-`, including overdue contacts, contacts with birthdays, and unmatched import candidates.
- No integration is connected (Google, Todoist, Telegram, WhatsApp, Mac hosts). `CRM_ENV=testing` cadences apply.
- Never drive an instance this checkout's `bin/launch` did not start.

## Driving conventions

- Write a drive script that imports `lib/pw.mjs` and run it with `bin/run <label> <script>`.
- Prefer ARIA roles and accessible names, labels, placeholders and `data-testid` over CSS selectors or DOM position. Quoted names below are exact.
- Records you create carry a unique whole-word marker (`Verify <stamp> ...`) so they never collide with seeded data or earlier runs.
- Navigate as a user would: nav links and buttons first, deep links only where the recipe says the URL is the entry point.

## Proof and skip reporting

- Use one `s.step` per user action so the screenshots show the action and the resulting state.
- Mutation proof includes a second, read-only view of the stored value: `bin/psql` or `s.apiGet`.
- Record the behavior IDs, the feature file and the entry point with the evidence directory you report.
- If an entry point is unreachable here (it needs a connected integration, a paired Mac, or real time passing), report the attempted path and the unmet precondition. Do not report it as verified through a different path.

## Feature entry contract

Each feature file starts with an H1 title and one paragraph naming the surface and the spec files that own its behavior, then exactly four H2 sections in this order.

1. `Sub-features` lists the behavior IDs the file can drive, each with a few words to find it, and names the IDs that cannot be reached here.
2. `How to get to it (user POV)` lists every user entry point.
3. `Driving it with Playwright` starts with `Preconditions:`, then pairs each user action with exact locators, the request to find in `api.jsonl`, and the read (`bin/psql`, `s.apiGet`) that shows the side effect. It names the behavior ID to judge against instead of stating the expected value.
4. `Gotchas` lists traps specific to driving this environment: locally missing integrations, dialogs, timing, caching, and spec waivers that change what counts as evidence here.

When adding a recipe for a behavior with no spec ID, add the behavior to `spec/` first under its maintenance rule, then cite it.

## Features

- [Dashboard](./dashboard.md) covers the overdue list, sorting, Log Interaction and Skip this cycle.
- [Contacts](./contacts.md) covers the list (search, filter, sort), create, detail, edit, delete and merge.
- [Birthdays](./birthdays.md) covers the read-only birthday sections.
- [Imports](./imports.md) covers import candidates (import, link, ignore) and method suggestions.
- [Settings](./settings.md) covers the integration sections, backup download, and Mac pairing, with the controls that must not be driven locally.
