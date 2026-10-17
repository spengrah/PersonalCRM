# Birthdays

The birthdays page groups contacts with a known birthday by how near it is. It is read-only. Expected behavior lives in `spec/contacts.yaml` and `spec/knowledge.yaml`; this file says how to drive it here.

## Sub-features

- `CON-045` grouping by proximity, soonest-first order, leap-day birthdays and the card link.
- `KNW-035` year-less birthdays.
- Not reachable on most days: `CON-045` `gift-planning-near-year-end`, which shows only in November and December of the real calendar.

## How to get to it (user POV)

- Choose the `Birthdays` nav link (`/birthdays`).
- Choose a card's name to reach that contact's detail page.

## Driving it with Playwright

Preconditions:

- Baseline from `README.md`. The `standard` world includes a contact whose year-less birthday (stored year 1900) is today, one whose birthday is tomorrow, and one born on February 29. Find them with `bin/psql -c "SELECT full_name, birthday FROM contact WHERE deleted_at IS NULL AND birthday IS NOT NULL AND (EXTRACT(YEAR FROM birthday) = 1900 OR to_char(birthday, 'MM-DD') = '02-29')"`.

- **Load.** Choose the `Birthdays` nav link, then wait for `getByRole('heading', { name: 'Birthday Tracker' })`. The box under the heading shows the clock the page computes from. The page's reads are `GET /api/v1/contacts?limit=1000` and `GET /api/v1/system/time` in `api.jsonl`.
- **Sections.** Each group is a `section` with an `h2`; locate one with `page.locator('section', { has: page.getByRole('heading', { name: /Upcoming Birthdays/ }) })`. The others are `/Today's Birthdays/`, `/Already Celebrated This Year/` and `/Gift Planning/`. Cards are `getByTestId('birthday-card')`; each holds a link with the contact name.
- **Year-less birthday.** In the `Today's Birthdays` section, read the year-less contact's card text. Judge it against `KNW-035`.
- **Leap day.** Read the February 29 contact's card. Judge it against `CON-045` `leap-day-next-occurrence`.
- **New birthday appears.** Create a `Verify <stamp>` contact with `getByLabel('Birthday')` set a few days after the date in the clock box (see `contacts.md`; the request is `POST /api/v1/contacts`), then return through the `Birthdays` nav link and find its card. Read the stored value with `bin/psql -c "SELECT full_name, birthday FROM contact WHERE full_name LIKE 'Verify %' AND deleted_at IS NULL"`. Judge the placement against `CON-045`.
- **Open contact.** Click the card's `getByRole('link', { name, exact: true })` and wait for `/contacts/<id>`.
- **Proof.** A full-page `s.shot` shows every section.

## Gotchas

- The verify instance runs without time acceleration (`s.apiGet('/api/v1/system/time')` reports `is_accelerated: false`), so the page groups by the browser's local calendar day, not the server clock. The browser's time zone moves contacts between groups near midnight. Use a birthday several days from today.
- Do not set a factor in the header's time-acceleration widget: it changes the clock for the whole instance.
- The E2E specs freeze the clock with `page.route`; a verification must not, so any date-dependent group (such as gift planning) appears only when the real date puts it there.
