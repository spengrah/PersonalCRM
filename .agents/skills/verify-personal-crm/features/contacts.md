# Contacts

Contacts is where the user finds, creates, edits, merges and deletes people, and opens a person's detail page. Expected behavior lives in `spec/contacts.yaml`, with the notepad in `spec/notes-meetings.yaml`, the interaction history in `spec/interactions.yaml` and knowledge fields in `spec/knowledge.yaml`; this file says how to drive it here.

## Sub-features

- List: `CON-023` search, `CON-054` cadence filter, `CON-057` column sorting, `CON-058` pagination, `CON-061` derived values, `CON-066` pagination reset.
- `CON-055` create from the form, `CON-002` and `CON-015` create validation.
- Detail: `CON-056` methods, `NTS-007` notepad, `KNW-034` and `KNW-035` knowledge fields, `IXN-001` interaction history, `CON-059` and `CON-060` previous/next, `CON-065` back to list.
- `CON-063` edit, `NTS-008` notepad edit.
- `CON-053` Log Interaction modal, `CON-044` Mark as Contacted from the list.
- `CON-042` delete confirmation, `CON-010` and `CON-024` the soft delete.
- `CON-043` merge flow, `CON-027` to `CON-035` and `CON-067` what a merge moves.

## How to get to it (user POV)

- Choose the `Contacts` nav link (`/contacts`).
- Choose `New Contact` on the list, or `Add Contact` / `Add New Contact` on the dashboard, to reach `/contacts/new`.
- Choose a contact's name in the list, the dashboard or the birthdays page to reach `/contacts/<id>`.
- Open a list row's `Contact actions` menu and choose `Edit` or `Merge` (`CON-041`).
- Deep link `/contacts?search=<term>`; the list state (`search`, `sort`, `order`, `cadence_filter`, `page`) lives in the URL.

## Driving it with Playwright

Preconditions:

- Baseline from `README.md`. `examples/create-contact.mjs` is a working recipe for `CON-055` and `CON-023`.

- **List.** `page.goto('/contacts')`, wait for `getByRole('heading', { name: 'Contacts', level: 2 })`. Column headers are buttons (`Name`, `Cadence`, `Location`, `Birthday`, `Last response`, `Next Contact`); the active one carries `aria-sort`.
- **Search.** Fill `getByPlaceholder('Search contacts...')`, then wait for `getByRole('link', { name, exact: true })`. Read the term rules in `CON-023` before choosing a term.
- **Filter.** `getByLabel('Filter by cadence').selectOption('has_cadence')` (values `''`, `has_cadence`, `no_cadence`).
- **Create.** Choose `getByRole('link', { name: 'New Contact' })`. Fill `getByLabel('Full Name')`. For a method, `getByRole('combobox', { name: 'Contact method type' }).first().selectOption('email')` and `getByRole('textbox', { name: 'Contact method value' }).first().fill(...)`; `Add method` adds a row. Optional fields: `getByLabel('Location')`, `getByLabel('Birthday')` (date input), the `Contact Cadence` select, `getByLabel('Notes')`. Click `getByRole('button', { name: 'Create Contact' })`, then wait for `page.waitForURL(/\/contacts\/[0-9a-f-]{36}$/)`. Find `POST /api/v1/contacts` in `api.jsonl`. Read the rows with `bin/psql -c "SELECT c.full_name, m.type, m.value, m.is_primary FROM contact c JOIN contact_method m ON m.contact_id = c.id WHERE c.id = '<id>'"`.
- **Detail.** The `h2` is the full name; header buttons are `Log Interaction`, `Edit`, `Merge`, `Delete`. `getByRole('button', { name: 'Previous contact' | 'Next contact' })` and `Back to list` navigate. The interaction history is `getByRole('list', { name: 'Interaction history' })` inside `getByRole('region', { name: 'Interactions' })`.
- **Edit.** Click `getByRole('button', { name: 'Edit' }).first()`; the heading becomes `Edit Contact`. Change fields, click `Update Contact`, then wait for the `Edit` button to return. A `getByTestId('save-report')` box means a save step failed; record its text. Find the `PUT /api/v1/contacts/<id>` and any `.../methods` and `.../notes` calls in `api.jsonl`.
- **Log interaction.** Click `Log Interaction`; in the `dialog`, choose a direction, optionally fill `getByTestId('log-interaction-date-input')`, click `getByRole('button', { name: 'Log', exact: true })`, and wait for the dialog to close. Read the new `interaction` row with `bin/psql`.
- **Mark as contacted (list).** In the row, click `getByRole('button', { name: 'Contact actions' })`, then `getByRole('menuitem', { name: 'Mark as Contacted' })`. Read the `POST .../interactions` request body in `api.jsonl`.
- **Delete.** Register `page.once('dialog', d => d.accept())`, then click `Delete`. Find `DELETE /api/v1/contacts/<id>` in `api.jsonl`, then read `s.apiGet('/api/v1/contacts/<id>')` and `bin/psql -c "SELECT deleted_at FROM contact WHERE id = '<id>'"`.
- **Merge.** Create two `Verify <stamp>` contacts first. On the target, click `Merge`; in `getByRole('dialog', { name: 'Merge Contacts' })` fill `getByPlaceholder('Search for a contact to merge...')`, choose `getByRole('option', { name: sourceName })`, then `getByRole('button', { name: /Merge Contacts/i })`. Find `GET .../merge/preview` and `POST /api/v1/contacts/<target>/merge` in `api.jsonl`. Read both contacts' rows, methods and interactions before and after.

## Gotchas

- Pressing Enter in the search box does nothing, and every keystroke refetches. Wait for the expected row, not for one response.
- Detail pages log three 404s for `/api/v1/contacts/<id>/tasks` because Todoist is not configured, so task behaviors (`CAD-030`, `CAD-031`, `CAD-033`) cannot be proven here.
- Delete and task removal use native `confirm()`. Without a registered dialog handler the click hangs.
- Previous/next navigation is disabled while editing.
- A fresh `goto('/contacts')` drops the list context that `Back to list` restores.
