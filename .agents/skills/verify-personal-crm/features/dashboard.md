# Dashboard

The dashboard lists contacts whose cadence is overdue and lets the user log an interaction or skip the current cycle from each card. Expected behavior lives in `spec/dashboard.yaml` and `spec/cadence-followup.yaml`; this file says how to drive it here.

## Sub-features

- `DSH-001` landing route, `DSH-002` global navigation, `DSH-003` add/browse paths, `DSH-004` loading and error states.
- `CAD-026` the overdue list, `CAD-027` its orderings, `CAD-023` the endpoint behind it.
- `CAD-046` the card's two actions, `CON-053` the Log Interaction modal, `CAD-006` the timestamps a logged direction moves.
- `CAD-044` skip this cycle.

## How to get to it (user POV)

- Open `/`; it resolves to `/dashboard`.
- Choose the `Dashboard` nav link. Below the `sm` width, open the hamburger menu first.
- Choose a card's name to leave for the contact detail page (`/contacts/<id>`).

## Driving it with Playwright

Preconditions:

- Baseline from `README.md`. The `standard` world has overdue contacts.

- **List.** `page.goto('/dashboard')`, then wait for `getByRole('heading', { name: 'Action Required' })`. The list is `getByRole('list', { name: 'Overdue contacts' })`; each card is a `listitem` whose `h3` heading is the contact name. Urgency dots are `getByRole('img', { name: 'High urgency' | 'Medium urgency' | 'Low urgency' | 'Awaiting reply' })`. Read the server's view with `s.apiGet('/api/v1/contacts/overdue')`.
- **Pick a card.** `page.getByRole('listitem').filter({ has: page.getByRole('heading', { name, exact: true }) })`. Read `name` from the first card's heading rather than hardcoding it.
- **Sort.** Click `getByRole('button', { name: 'Most Urgent' | 'Name' | 'Last Contacted' })`, then read the card headings in order.
- **Log interaction.** On the card, click `getByRole('button', { name: 'Log Interaction', exact: true })`. In the `dialog`, choose a direction button (`Mutual`, `Outbound`, `Inbound`; the pressed one has `aria-pressed="true"`), optionally fill `getByTestId('log-interaction-date-input')`, then click `getByRole('button', { name: 'Log', exact: true })`. Find the `POST /api/v1/contacts/<id>/interactions` in `api.jsonl`. Read the side effect with `bin/psql -c "SELECT direction, occurred_at FROM interaction WHERE contact_id = '<id>' ORDER BY created_at DESC LIMIT 1"` and `bin/psql -c "SELECT last_contacted, last_interaction_at, last_outreach_at, last_response_at, contact_by FROM contact WHERE id = '<id>'"`, captured before and after. Judge both against `CAD-006` and `CAD-046`.
- **Skip this cycle.** On a card, click `getByRole('button', { name: 'Skip this cycle', exact: true })`. Find the `POST /api/v1/contacts/<id>/skip` and its response body in `api.jsonl`. Read `contact_by`, `last_skipped_at` and `awaiting_reply_until` from `contact`, and the contact's `interaction` rows, before and after. Judge against `CAD-044`.
- **Proof.** One `s.step` per click, an `s.shot` of the settled list, and the before/after `bin/psql` reads recorded with `s.note`.

## Gotchas

- `CRM_ENV=testing` compresses cadences (`CAD-003`), so a rendered "day" is seconds and `days overdue` values are huge. Compare order and membership, not day counts.
- `CAD-045` and `CAD-046` carry waivers for testing-mode cadence. Read them before proving a skip: under `testing` the card is not the evidence, the response and the database are.
- A failed log or skip shows nothing in the UI (`DSH-006` is proposed, not current). `api.jsonl` is where a failure shows.
- The overdue query is cached for 5 minutes. After changing data outside the page, reload instead of waiting for a refetch.
