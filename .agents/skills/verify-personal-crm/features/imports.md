# Imports

Imports is where the user resolves people discovered by external sources and new contact methods suggested for existing contacts. Expected behavior lives in `spec/imports-matching.yaml`; this file says how to drive it here.

## Sub-features

- `IMP-026` the two-tab hub, `IMP-022` the unified suggestions feed.
- `IMP-027`, `IMP-028` and `IMP-039` the resolver modal.
- `IMP-012` import as new, `IMP-011` its guard.
- `IMP-013` link, `IMP-029` link confidence, `IMP-031` and `IMP-040` dependent surfaces updating.
- `IMP-007` the match-status lifecycle, including ignore.
- `IMP-030` and `IMP-018` method suggestions.
- `IMP-024` and `IMP-047` name candidates, `IMP-038` session deep links.

## How to get to it (user POV)

- Choose the `Imports` nav link (`/imports`, People tab).
- Choose the `Interactions` tab, or open `/imports?tab=interactions`.

## Driving it with Playwright

Preconditions:

- Baseline from `README.md`. The `standard` world seeds unmatched Google Contacts candidates and method suggestions; list them with `bin/api GET '/api/v1/imports/suggestions?limit=5'`.

- **Load.** `page.goto('/imports')`. Tabs are in `getByRole('tablist', { name: 'Imports sections' })`. Source filters are buttons in the group `Filter by source`, with `aria-pressed`.
- **Pick a candidate.** Read a candidate name from `s.apiGet('/api/v1/imports/suggestions')` (entries with `kind: "contact"`). The card is `page.locator('div.border', { has: page.getByRole('heading', { name, exact: true }) })`. The feed is paginated and re-sorted on every fetch; search pages for the heading rather than assuming position.
- **Ignore.** Click `card.getByRole('button', { name: 'Ignore candidate' })`. Find `POST /api/v1/imports/<id>/ignore` in `api.jsonl`. Read `bin/psql -c "SELECT match_status, crm_contact_id FROM external_contact WHERE id = '<id>'"`.
- **Import.** Click the card's `Import` button, or open `getByRole('dialog', { name: 'Resolve import candidate' })`, choose `Import as New`, then `Import as New Contact`. Find `POST /api/v1/imports/<id>/import` in `api.jsonl`. Read the `external_contact` row and the `contact` row its `crm_contact_id` points at.
- **Link.** In the resolver choose `Link to Existing`, search with `getByPlaceholder('Search for a contact...')`, pick a contact, click `Link Contact` (or use the card's `Link to <name> (NN%)` button). Find `POST /api/v1/imports/<id>/link` in `api.jsonl`. Read the `external_contact` row and the chosen contact's `contact_method` rows.
- **Method suggestions.** On a method-suggestion card click `Review`; in `getByRole('dialog', { name: 'Review method suggestions' })` use `Select method` / `Set as primary`, then `Confirm`. `Dismiss` on the card is the other path. Find `POST /api/v1/imports/suggestions/<id>/methods/resolve` or `.../dismiss` in `api.jsonl`, and read the contact's `contact_method` rows.
- **Proof.** One `s.step` per click; assert the green banner straight after the click, then record the `bin/psql` reads with `s.note`.

## Gotchas

- Banners auto-dismiss after 5 seconds. Assert them straight after the click, or rely on the database read.
- Never click `Sync Contacts`, `Sync Calendar` or other sync triggers. With no Google account they only show an error; with one they would reach Google.
- The Interactions tab's `Open Anarlog` link is a `hyprnote://` deep link; do not click it.
- The Interactions tab and the name-candidate section appear only when the world holds such records. If none exist, report those behaviors as unreachable rather than seeding through test routes.
- The E2E imports specs mock suggestions and accounts with `page.route`; a verification uses the seeded rows.
