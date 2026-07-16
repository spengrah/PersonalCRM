import { test, expect, type Page } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

// The Notes row in the contact-detail definition list. Scoped to the row so
// presence/absence assertions can never collide with other page text.
const notesRow = (page: Page) =>
  page.locator('dl > div').filter({ has: page.getByText('Notes', { exact: true }) })

test.describe('Contact notepad @area:contacts', () => {
  let testApi: TestAPI
  let contactId: string
  let fullName: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    const { ids } = await testApi.seedContacts([{ full_name: 'Notepad Contact' }])
    contactId = ids[0]
    fullName = `${testApi.prefix}-Notepad Contact`
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('shows the notepad only with content, preserving line breaks and clamping overflow', async ({
    page,
  }) => {
    // With no note seeded, the detail page renders no Notes row at all
    // spec: NTS-007[0]
    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
    await expect(notesRow(page)).toHaveCount(0)

    // A short note renders the row with its body, and no expand control
    // (the control appears only when the content overflows the clamp)
    // spec: NTS-007[0], NTS-007[2]
    const shortNote = `${testApi.prefix} short note body`
    await testApi.seedContactNote(contactId, shortNote)
    await page.reload()
    await expect(notesRow(page)).toBeVisible({ timeout: 15000 })
    await expect(notesRow(page)).toContainText(shortNote)
    await expect(notesRow(page).getByRole('button', { name: /Show more/i })).not.toBeVisible()

    // A long multi-paragraph note overflows the clamp: the expand control
    // appears, toggles open and closed, and the expanded body preserves the
    // seeded blank-line paragraph separation
    const firstParagraph = `${testApi.prefix} first paragraph with plenty of background context about the contact`
    const lastParagraph = `${testApi.prefix} closing paragraph noting the follow-up we agreed on`
    const longNote = [
      firstParagraph,
      'Second paragraph adding more detail so the body clearly exceeds the four-line clamp.',
      'Third paragraph continuing the story with even more detail about shared projects.',
      'Fourth paragraph covering their preferences, interests, and recent conversations.',
      lastParagraph,
    ].join('\n\n')
    await testApi.seedContactNote(contactId, longNote)
    await page.reload()
    await expect(notesRow(page)).toBeVisible({ timeout: 15000 })

    // spec: NTS-007[2]
    const expand = notesRow(page).getByRole('button', { name: /Show more/i })
    await expect(expand).toBeVisible()
    await expand.click()
    const collapse = notesRow(page).getByRole('button', { name: /Show less/i })
    await expect(collapse).toBeVisible()

    // spec: NTS-007[1]
    const body = await notesRow(page).locator('dd > div').first().innerText()
    expect(body).toContain(firstParagraph)
    expect(body).toContain(lastParagraph)
    expect(body).toContain('\n\n')

    await collapse.click()
    await expect(notesRow(page).getByRole('button', { name: /Show more/i })).toBeVisible()
  })

  test('saves the notepad as its own operation alongside the contact update', async ({ page }) => {
    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    await page.getByRole('button', { name: 'Edit' }).first().click()
    await expect(page.getByLabel('Notes')).toBeVisible()

    const renamed = `${testApi.prefix}-Renamed Notepad Contact`
    const noteBody = `${testApi.prefix} refreshed notepad body`
    await page.getByLabel('Full Name').fill(renamed)
    await page.getByLabel('Notes').fill(noteBody)

    // Hold the notepad PUT open at the network layer so the ordering claim is
    // observable: if edit mode closed after only the contact PUT (a
    // fire-and-forget notepad save), the still-open assertion below would
    // catch it. Non-PUT traffic on the notes path passes through untouched.
    let releaseNotesPut = () => {}
    const notesGate = new Promise<void>(resolve => {
      releaseNotesPut = resolve
    })
    await page.route(`**/api/v1/contacts/${contactId}/notes`, async route => {
      if (route.request().method() !== 'PUT') return route.continue()
      await notesGate
      return route.continue()
    })

    // Submitting fires TWO separate PUTs — the contact update and the notepad
    // save — and edit mode closes only once BOTH have completed
    const contactPut = page.waitForResponse(
      r => r.url().endsWith(`/api/v1/contacts/${contactId}`) && r.request().method() === 'PUT'
    )
    const notesPut = page.waitForResponse(
      r => r.url().endsWith(`/api/v1/contacts/${contactId}/notes`) && r.request().method() === 'PUT'
    )
    await page.getByRole('button', { name: 'Update Contact' }).click()

    // The contact update has landed while the notepad save is still held
    // open — the edit form must still be on screen
    // spec: NTS-008[0]
    const contactResponse = await contactPut
    expect(contactResponse.ok()).toBe(true)
    await expect(page.getByRole('button', { name: 'Update Contact' })).toBeVisible()

    // Release the notepad save; only now may edit mode close
    releaseNotesPut()
    const notesResponse = await notesPut
    expect(notesResponse.ok()).toBe(true)
    // spec: NTS-008[0]
    await expect(page.getByRole('button', { name: 'Edit' }).first()).toBeVisible({
      timeout: 15000,
    })

    // Both new values render after the save
    // spec: NTS-008[2]
    await expect(page.getByRole('heading', { name: renamed })).toBeVisible()
    await expect(notesRow(page)).toContainText(noteBody)
  })

  test('clearing the notepad removes the note', async ({ page }) => {
    const disposable = `${testApi.prefix} disposable note body`
    await testApi.seedContactNote(contactId, disposable)

    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
    await expect(notesRow(page)).toContainText(disposable)

    await page.getByRole('button', { name: 'Edit' }).first().click()
    await expect(page.getByLabel('Notes')).toBeVisible()
    await page.getByLabel('Notes').fill('')

    // An empty notepad submission deletes the note: the API answers 204
    // spec: NTS-008[1]
    const [notesResponse] = await Promise.all([
      page.waitForResponse(
        r =>
          r.url().endsWith(`/api/v1/contacts/${contactId}/notes`) && r.request().method() === 'PUT'
      ),
      page.getByRole('button', { name: 'Update Contact' }).click(),
    ])
    expect(notesResponse.status()).toBe(204)

    // Back on the detail view, the Notes row is gone
    // spec: NTS-008[1]
    await expect(page.getByRole('button', { name: 'Edit' }).first()).toBeVisible({
      timeout: 15000,
    })
    await expect(notesRow(page)).toHaveCount(0)
  })
})
