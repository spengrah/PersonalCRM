# Imports

Imports is where the user resolves people discovered by external sources and new contact methods suggested for existing contacts. Expected behavior lives in `spec/imports-matching.yaml`; this file says how to drive it here.

## Sub-features

- `IMP-026` the two-tab hub and the low-confidence names section, `IMP-022` the unified suggestions feed.
- `IMP-027`, `IMP-028` and `IMP-039` the resolver modal.
- `IMP-012` import as new, `IMP-014` link-only candidates.
- `IMP-013` link, `IMP-029` link confidence, `IMP-031` and `IMP-040` dependent surfaces updating.
- `IMP-007` the match-status lifecycle, including ignore.
- `IMP-030` and `IMP-018` method suggestions.
- `IMP-024` title-derived name candidates, `IMP-047` nameless candidates, `IMP-038` session deep links.
- Not reachable here: the server-side refusals of `IMP-011` (the UI never offers the refused action), and the conflict cards on the Interactions tab (the `standard` world seeds only orphan meeting notes).

## How to get to it (user POV)

- Choose the `Imports` nav link (`/imports`, People tab). `/imports?tab=needs-attention` is an alias for it.
- Choose the `Interactions` tab, or open `/imports?tab=interactions`.
- A paired Mac's notification opens `/imports?tab=interactions&session=<anarlog_session_id>`. Without a Mac, open that URL directly; it is the entry point under test.

## Driving it with Playwright

Preconditions:

- Baseline from `README.md`. The `standard` world seeds Google Contacts, Gmail, calendar and WhatsApp candidates, one method suggestion, one title-derived name group and two orphan meeting notes. List them with `bin/api GET '/api/v1/imports/suggestions?limit=20'` and `bin/api GET /api/v1/meeting-notes/needs-attention`.

- **Load.** Choose the `Imports` nav link. Tabs are in `getByRole('tablist', { name: 'Imports sections' })`. Source filters are buttons in `getByRole('group', { name: 'Filter by source' })`, with `aria-pressed`.
- **Pick a candidate.** Read a candidate from `s.apiGet('/api/v1/imports/suggestions')` (entries with `kind: "contact"`; the id is `candidate.id`). The card is `page.getByTestId('candidate-card').filter({ has: page.getByRole('heading', { name, exact: true }) })`. The feed is paginated and re-sorted on every fetch; search pages for the heading rather than assuming position. A candidate whose `allowed_actions` lacks `import` shows no `Import` button.
- **Ignore.** Click `card.getByRole('button', { name: 'Ignore candidate' })`; it posts at once. Find `POST /api/v1/imports/<id>/ignore` in `api.jsonl`. Read `bin/psql -c "SELECT match_status, crm_contact_id FROM external_contact WHERE id = '<id>'"`.
- **Import.** The card's `Import` button opens `getByRole('dialog', { name: 'Resolve import candidate' })` and posts nothing. In the dialog click `getByRole('button', { name: 'Import as New Contact' })` (`Import as New` without `exact: true` matches both mode buttons). Find `POST /api/v1/imports/<id>/import` in `api.jsonl`. Read the `external_contact` row and the `contact` row its `crm_contact_id` points at.
- **Link.** The card's `Link to <name> (NN%)` button (`Link (select)` without a suggested match) opens the dialog in link mode with the match already chosen; click `Link Contact`. To choose another contact, click the selector's `Search for a contact...` text to open it, fill `getByPlaceholder('Search for a contact...')`, and pick a `getByRole('option')`. Find `POST /api/v1/imports/<id>/link` in `api.jsonl`. Read the `external_contact` row and the chosen contact's `contact_method` rows. For `IMP-040`, open the chosen contact's detail page before linking and return to it by client-side navigation afterwards.
- **Method suggestions.** On `getByTestId('method-suggestion-card')` click `Review`. In `getByRole('dialog', { name: 'Review method suggestions' })` every method starts selected; `Deselect method` toggles one off. Click `Confirm`, or the dialog's `Dismiss`; the card's `Dismiss` is the other path. Find `POST /api/v1/imports/suggestions/<external_contact_id>/methods/resolve` or `.../dismiss` in `api.jsonl`. Read the contact's `contact_method` rows and `bin/psql -c "SELECT pending_method_suggestions, dismissed_method_suggestions FROM external_contact WHERE id = '<external_contact_id>'"`.
- **Name candidates.** Below the candidates, `Names found in session titles` lists rows `page.locator('[data-testid^="name-candidate-row-"]')` (the suffix is the normalized token). Click `Not a person` or `Create contact…` (which opens `getByRole('dialog', { name: 'Create contact from name candidate' })`). Find `POST /api/v1/imports/anarlog-title/resolve` in `api.jsonl` and read `bin/psql -c "SELECT match_status, count(*) FROM external_contact WHERE source = 'anarlog_title' GROUP BY 1"`.
- **Session deep link and orphan notes.** `page.goto('/imports?tab=interactions&session=<anarlog_session_id>')`, wait for `getByTestId('orphan-card')`, and check that `session` has left the URL. On the session's card click `Log as impromptu`. Find `POST /api/v1/meeting-notes/<id>/resolve-link` in `api.jsonl` and read `bin/psql -c "SELECT linkage_state FROM meeting_note WHERE id = '<id>'"`.
- **Proof.** One `s.step` per click; assert the banner text straight after the click, then record the `bin/psql` reads with `s.note`.

## Gotchas

- After an import, link or ignore, the resolver dialog moves to the next candidate instead of closing (`IMP-028`). Close it with `Escape` or `Cancel`.
- Banners have no role and auto-dismiss after 5 seconds. Find them by text (for example "Contact linked successfully!") straight after the click, or rely on the database read.
- Never click `Sync Contacts`, `Sync Calendar` or other sync triggers. With no Google account they only show an error; with one they would reach Google.
- The Interactions tab's `Open Anarlog` link is a `hyprnote://` deep link; do not click it.
- The `standard` world seeds one method suggestion. After resolving or dismissing it, relaunch to drive the other path.
- Some E2E imports specs mock suggestions and accounts with `page.route`; a verification uses the seeded rows.
