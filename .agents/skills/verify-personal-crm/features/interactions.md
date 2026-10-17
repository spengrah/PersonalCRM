# Interactions

The Interactions page lists every interaction across all contacts, filters it by type, contact and judgment run, and is where the user reviews a run's proposals and corrects rows. Expected behavior lives in `spec/interactions.yaml`, with the judgment review route and reads in `spec/knowledge.yaml`; this file says how to drive it here.

## Sub-features

- `IXN-041` the list, its rows and pagination, `IXN-051` the route behind it.
- `IXN-042` the type, contact and run filters and the address that holds them.
- `IXN-043` a run's summary and incomplete threads.
- `IXN-044` the review indicator, `IXN-045` the proposal beneath its cut-off, `IXN-046` the proposal's items, `IXN-052` the judgment the list carries.
- `IXN-047` inline review, `IXN-048` reviews shown after a reload, `IXN-049` an accepted big moment, `KNW-049` and `KNW-050` the routes behind them.
- `IXN-037` remove and `IXN-038` merge from a row (the same controls as the contact detail history).
- `IXN-050` the link in from a contact's history.
- Not reachable here: proposal items in the `live` and `suggested` states, and the `Accept`/`Keep`/`Keep original` corrections of `IXN-047`. The instance names no production decision method, so every seeded item is `not_acting`.

## How to get to it (user POV)

- Choose the `Interactions` nav link (`/interactions`). Below the `sm` width, open `getByRole('button', { name: 'Main menu' })` first.
- On a contact's detail page, choose `Review in all interactions` to reach `/interactions?contact=<id>`.
- The filters live in the address: `/interactions?contact=<id>&type=<type>&run=<run id>&page=<n>`.

## Driving it with Playwright

Preconditions:

- Baseline from `README.md`. The `standard` world seeds interactions but no judgment runs, and no method can run here. Record runs before loading the page through the testing-only route, as a method would: `bin/api POST /api/v1/test/judgment-runs '<json>'`. The body's shape is `SeedRun` in `frontend/tests/e2e/helpers/bench.ts`, plus `wording_version`. A judgment's `thread_interaction_id` is a venue-backed interaction's id: take a chat or email row's `item.identity.id` from `bin/api GET '/api/v1/interactions?type=message&limit=5'`, and its `cut_off` from the row's latest message `sent_at` (`GET /api/v1/timeline-items/<kind>/<id>/content?contact_id=<context_contact_id>`). Add `"unfinished": true` for a run still in progress. The route answers `201` with `run_id`.

- **List.** Choose the `Interactions` nav link, then wait for `getByRole('list', { name: 'Interactions' })`. Rows are `listitem`s carrying `data-interaction-id`. Compare with `s.apiGet('/api/v1/interactions?limit=20')`. Page with `getByTestId('pagination').getByRole('button', { name: 'Next' })` and `waitForURL(/page=2/)`.
- **Type.** Click a button in `getByRole('group', { name: 'Type' })` (`All`, `Messages`, `Email`, `Calendar`, `Calls`, `Other`); the active one has `aria-pressed="true"`, and the address gains `type=` and drops `page=`.
- **Contact.** In `getByRole('group', { name: 'Contact' })`, click the text `Search contacts...` to open the selector, fill `getByPlaceholder('Search contacts...')`, and choose `getByRole('option', { name })` (not exact: the option's name also holds the primary method). The address gains `contact=`.
- **Run.** `getByLabel('Run', { exact: true }).selectOption(<run id>)`. The address gains `run=`; `getByRole('region', { name: 'Run summary' })` states the run, and `getByRole('list', { name: 'Incomplete threads' })` lists its incomplete threads. Compare with `s.apiGet('/api/v1/judgment-runs')` and `s.apiGet('/api/v1/interactions?run_id=<id>')`.
- **Clear.** `getByRole('button', { name: 'Clear filters' }).first()` returns to `/interactions`.
- **Proposal.** On the judged row (`[data-interaction-id="<id>"]`, with `[data-review-indicator]`), click `Expand content`, then wait for `[data-cut-off]`. The proposal is `getByRole('region', { name: /^Proposal <method_key>/ })`; a non-production proposal may start collapsed behind `Show proposal`. Items are `li[data-item]` with `data-state`, in the groups `I owe a reply`, `Owes me a reply`, `Commitments` and `Big moment`.
- **Review.** Click `Confirm <contact name>` or `Remove <contact name>`, and `Confirm big moment` or `Remove big moment`. Find `POST /api/v1/judgment-outcomes/<id>/review` and its body in `api.jsonl`. Read `bin/psql -c "SELECT value_text FROM assertion WHERE predicate_key = 'judgment_review' ORDER BY knowledge_from DESC LIMIT 5"`. Reload, expand again and read `[data-review-mark]`.
- **Remove a row.** On a row, click `More actions`, then `getByRole('menuitem', { name: 'Remove' })`. In `getByRole('dialog', { name: 'Remove interaction' })` choose a reason radio (match the start of its name, such as `/^Duplicate/`), then click `Remove`. Find `GET /api/v1/interactions/<id>/participants` and `POST /api/v1/interactions/<id>/removals` in `api.jsonl`, and compare the contact's total from `s.apiGet('/api/v1/interactions?limit=1&contact_id=<id>')` before and after.
- **Merge rows.** Filter to a contact with several rows. On one row choose `More actions` → `Merge`; `getByRole('region', { name: 'Merge selection' })` appears. Check `Select interaction` on another row, click `Review merge`, wait for `[data-merge-result]` in `getByRole('dialog', { name: 'Merge interactions' })`, then click `Merge` (exact). Find `POST /api/v1/interactions/merge/preview` and `POST /api/v1/interactions/merge` in `api.jsonl`, and compare the contact's total before and after.
- **Link in.** Open `/contacts/<id>`, choose `Review in all interactions`, and check that the address holds `contact=<id>` and the Contact group shows the contact.

## Gotchas

- The verify instance sets no `DECISION_PRODUCTION_METHOD`, so proposals from any method are non-production and their items are `not_acting`. Judge item state against `IXN-052`.
- A rejected `POST /api/v1/test/judgment-runs` (for example an unknown `thread_interaction_id`) still leaves an unfinished run behind, which then appears in the Run select. Relaunch for a clean run list.
- A run's window shows in the browser's time zone, so a `from` at UTC midnight can read as the previous day.
- Removals and merges cannot be undone, and recorded runs persist until cleanup. Relaunch for a clean world.
- Only rows of kind `interaction` offer `More actions`; grouped chat rows (`message_burst`) do not.
