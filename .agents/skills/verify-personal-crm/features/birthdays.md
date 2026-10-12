# Birthdays

The birthdays page groups contacts with a known birthday by how near it is. It is read-only. Expected behavior lives in `spec/contacts.yaml` and `spec/knowledge.yaml`; this file says how to drive it here.

## Sub-features

- `CON-045` grouping by proximity.
- `KNW-035` year-less birthdays.

## How to get to it (user POV)

- Choose the `Birthdays` nav link (`/birthdays`).
- Choose a card's name to reach that contact's detail page.

## Driving it with Playwright

Preconditions:

- Baseline from `README.md`. The `standard` world includes contacts with birthdays.

- **Load.** `page.goto('/birthdays')`, wait for `getByRole('heading', { name: 'Birthday Tracker' })`. The box under the heading shows the clock the page computes from.
- **Sections.** Each group is a `section` with an `h2`; locate one with `page.locator('section', { has: page.getByRole('heading', { name: /Upcoming Birthdays/ }) })`. Cards are `getByTestId('birthday-card')`; each holds a link with the contact name.
- **New birthday appears.** Create a `Verify <stamp>` contact with `getByLabel('Birthday')` set a few days after the date in the clock box (see `contacts.md`), then return to `/birthdays` and find its card. Read the stored value with `bin/psql -c "SELECT full_name, birthday FROM contact WHERE full_name LIKE 'Verify %' AND deleted_at IS NULL"`. Judge the placement against `CON-045`.
- **Open contact.** Click the card's `getByRole('link', { name, exact: true })` and wait for `/contacts/<id>`.
- **Proof.** A full-page `s.shot` shows every section; the page's data reads are in `api.jsonl`.

## Gotchas

- The page groups by local calendar day on the accelerated clock (`GET /api/v1/system/time`), so the machine's time zone moves contacts between groups near midnight. Use a birthday several days from today.
- The E2E specs freeze the clock with `page.route`; a verification must not, so any date-dependent group (such as gift planning) appears only when the real date puts it there.
