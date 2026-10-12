// Reference drive: create a contact through /contacts/new, then prove it from
// the UI (detail page, list search) and the backend (API read).
// Run: .ai/skills/verify-personal-crm/bin/run create-contact .ai/skills/verify-personal-crm/examples/create-contact.mjs
// Then prove the DB side effect with bin/psql (see features/contacts.md).
import { drive } from '../lib/pw.mjs'

const stamp = Date.now().toString(36)
const name = `Verify ${stamp} Person` // whole words: list search is full-text
const email = `verify-${stamp}@example.test`

await drive(async s => {
  const { page } = s
  s.note('inputs', { name, email })

  await s.step('contacts-list', async p => {
    await p.goto('/contacts')
    await p.getByRole('heading', { name: 'Contacts', level: 2 }).waitFor()
  })

  await s.step('new-contact-form-filled', async p => {
    await p.getByRole('link', { name: 'New Contact' }).click()
    await p.getByLabel('Full Name').fill(name)
    await p.getByRole('combobox', { name: 'Contact method type' }).first().selectOption('email')
    await p.getByRole('textbox', { name: 'Contact method value' }).first().fill(email)
  })

  const id = await s.step('created-detail-page', async p => {
    await p.getByRole('button', { name: 'Create Contact' }).click()
    await p.waitForURL(/\/contacts\/[0-9a-f-]{36}$/)
    await p.getByRole('heading', { name, exact: true }).waitFor()
    return p.url().split('/').pop()
  })
  s.note('contact id', id)
  s.check(await page.getByText(email).isVisible(), 'detail page shows the email method')

  await s.step('list-search-finds-it', async p => {
    await p.goto('/contacts')
    await p.getByPlaceholder('Search contacts...').fill(`Verify ${stamp}`)
    await p.getByRole('link', { name, exact: true }).waitFor()
  })

  const { status, data } = await s.apiGet(`/api/v1/contacts/${id}`)
  s.check(status === 200, `GET /api/v1/contacts/${id} → 200`)
  s.check(JSON.stringify(data).includes(email), 'API contact carries the email method')
})
