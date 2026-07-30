import { test, expect, type APIRequestContext, type Page } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

// Plain @playwright/test, deliberately NOT the custom fixture: that fixture's
// query client sets staleTime: 0, which refetches the contact detail constantly
// and hands the form a fresh server picture — destroying the very staleness
// these tests exist to survive.
//
// The incident: enrichment added a method server-side, the open edit form held
// a stale list, and saving wholesale-deleted and recreated from that stale
// list, permanently destroying the enriched method. @area:contacts

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = { 'X-API-Key': API_KEY, 'Content-Type': 'application/json' }

interface MethodRow {
  id: string
  type: string
  value: string
  is_primary: boolean
}

async function getMethods(request: APIRequestContext, contactId: string): Promise<MethodRow[]> {
  const res = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
    headers: API_HEADERS,
  })
  expect(res.ok()).toBe(true)
  const body = await res.json()
  return (body?.data?.methods ?? []) as MethodRow[]
}

/**
 * The value the DECLARATION seeded for one method kind. The generator owns it
 * (namespace-prefixed email, namespace-blocked phone), so every assertion reads
 * it back rather than restating a literal that would silently stop matching.
 */
async function seededMethodValue(
  request: APIRequestContext,
  contactId: string,
  type: string
): Promise<string> {
  const method = (await getMethods(request, contactId)).find(m => m.type === type)
  expect(method, `the declared contact should carry a seeded ${type}`).toBeTruthy()
  return method!.value
}

function seededEmail(request: APIRequestContext, contactId: string): Promise<string> {
  return seededMethodValue(request, contactId, 'email')
}

/** Adds a method out of band, the way another writer (enrichment) would. */
async function addMethodOutOfBand(
  request: APIRequestContext,
  contactId: string,
  type: string,
  value: string
) {
  const res = await request.post(`${API_BASE_URL}/api/v1/contacts/${contactId}/methods`, {
    headers: API_HEADERS,
    data: { operations: [{ op: 'add', type, value }] },
  })
  expect(res.ok(), 'out-of-band add should succeed').toBe(true)
}

async function openEditMode(page: Page, contactId: string) {
  await page.goto(`/contacts/${contactId}?action=edit`)
  await page.waitForLoadState('domcontentloaded')
  await expect(page.getByRole('heading', { name: 'Edit Contact' })).toBeVisible({ timeout: 15000 })
}

function methodValues(page: Page) {
  return page.getByRole('textbox', { name: 'Contact method value' })
}

/** Forces the notes step to fail, so the methods step lands and the form stays open. */
async function failNotesStep(page: Page, contactId: string) {
  await page.route(`**/api/v1/contacts/${contactId}/notes`, async route => {
    if (route.request().method() === 'PUT') {
      await route.fulfill({
        status: 500,
        contentType: 'application/json',
        body: JSON.stringify({ success: false, error: { code: 'TEST', message: 'forced' } }),
      })
      return
    }
    await route.continue()
  })
}

async function save(page: Page) {
  await page.getByRole('button', { name: 'Update Contact' }).click()
}

// A second save must be observed by ITS OWN request landing. Asserting on the
// save-report alone would pass trivially: the banner is still on screen from
// the previous attempt, so the assertion can resolve before the retry has done
// anything at all.
async function saveAndAwait(page: Page, contactId: string, step: 'methods' | 'notes') {
  const path =
    step === 'methods'
      ? `/api/v1/contacts/${contactId}/methods`
      : `/api/v1/contacts/${contactId}/notes`
  const method = step === 'methods' ? 'POST' : 'PUT'
  const responsePromise = page.waitForResponse(res => {
    return new URL(res.url()).pathname === path && res.request().method() === method
  })
  await save(page)
  return responsePromise
}

test.describe('Contact method preservation @area:contacts', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('preserves an unseen method while the user edits a visible one', async ({
    page,
    request,
  }) => {
    // spec: CON-063.method-added-another-writer
    const seeded = await testApi.seedBehavior('CON-063')
    const contactId = seeded.entities['target'].id
    const emailB = `pres-b-${Date.now()}@example.com`

    await openEditMode(page, contactId)

    // Another writer adds B AFTER the form opened, so B is genuinely unseen.
    await addMethodOutOfBand(request, contactId, 'phone', '5555550142')

    // Edit the visible method. Changing only a scalar would skip the methods
    // request entirely, and the test would pass even against a derivation that
    // deletes unseen methods whenever any method is edited.
    await methodValues(page).first().fill(emailB)
    await save(page)

    await expect(page.getByRole('heading', { name: 'Edit Contact' })).toBeHidden({ timeout: 15000 })

    const methods = await getMethods(request, contactId)
    expect(methods.map(m => m.value).sort()).toEqual([emailB, '5555550142'].sort())
  })

  test('removes a method the user deleted in the form', async ({ page, request }) => {
    // spec: CON-063.method-user-explicitly-deleted
    // The two-method entity: this is the one citing test whose subject has to
    // carry a second method BEFORE the form opens, so there is something the user
    // can explicitly delete.
    const seeded = await testApi.seedBehavior('CON-063')
    const contactId = seeded.entities['two-methods'].id
    const emailA = await seededEmail(request, contactId)
    const seededPhone = await seededMethodValue(request, contactId, 'phone')

    await openEditMode(page, contactId)
    await expect(methodValues(page)).toHaveCount(2)

    // Located by live input VALUE, not by a [value=...] CSS selector: React
    // sets the value as a DOM property and never writes the attribute, so the
    // attribute selector matches nothing.
    const rowIndex = await methodValues(page).evaluateAll(
      (inputs, target) => inputs.findIndex(i => (i as HTMLInputElement).value === target),
      seededPhone
    )
    expect(rowIndex, 'the seeded phone row should be on screen').toBeGreaterThanOrEqual(0)
    await page.getByRole('button', { name: 'Remove' }).nth(rowIndex).click()
    await save(page)

    await expect(page.getByRole('heading', { name: 'Edit Contact' })).toBeHidden({ timeout: 15000 })

    const methods = await getMethods(request, contactId)
    expect(methods.map(m => m.value)).toEqual([emailA])
  })

  test('reports which parts of the save succeeded when one step fails', async ({ page }) => {
    // spec: CON-063.partial-failure-names-saved-parts
    const seeded = await testApi.seedBehavior('CON-063')
    const contactId = seeded.entities['target'].id

    await openEditMode(page, contactId)
    await failNotesStep(page, contactId)

    await methodValues(page).first().fill(`part-b-${Date.now()}@example.com`)
    await save(page)

    const report = page.getByTestId('save-report')
    await expect(report).toBeVisible({ timeout: 15000 })
    // Names what landed rather than reporting a generic failure — the user's
    // next action differs between "nothing saved" and "the note did not save".
    await expect(report).toContainText('contact methods')
    await expect(report).toContainText('the note could not be saved')
    await expect(report).not.toContainText('Nothing was saved')
  })

  test('a retry after a partial failure does not undo the saved parts', async ({
    page,
    request,
  }) => {
    // spec: CON-063.retrying-after-partial-failure
    const seeded = await testApi.seedBehavior('CON-063')
    const contactId = seeded.entities['target'].id
    const emailB = `retry-b-${Date.now()}@example.com`

    await openEditMode(page, contactId)
    await failNotesStep(page, contactId)

    await methodValues(page).first().fill(emailB)
    await save(page)
    await expect(page.getByTestId('save-report')).toBeVisible({ timeout: 15000 })

    // Retry unedited. The methods step already landed and must stay landed, so
    // the retry resumes at notes and issues no methods request at all.
    await saveAndAwait(page, contactId, 'notes')
    await expect(page.getByTestId('save-report')).toBeVisible({ timeout: 15000 })

    const methods = await getMethods(request, contactId)
    expect(methods.map(m => m.value)).toEqual([emailB])
  })

  test('a method added in a partially failed save can then be removed', async ({
    page,
    request,
  }) => {
    // spec: CON-063.partial-save-add-can-be-removed
    //
    // The ordering here is load-bearing. B is added AFTER edit mode opens, so it
    // is genuinely unseen; adding it before would make it legitimately part of
    // the acknowledged state, and an implementation that assigns the whole
    // response method list would pass the pin written to catch it.
    const phoneC = '5555550144'
    const seeded = await testApi.seedBehavior('CON-063')
    const contactId = seeded.entities['target'].id
    const emailA = await seededEmail(request, contactId)

    await openEditMode(page, contactId)
    await addMethodOutOfBand(request, contactId, 'phone', '5555550145')
    await failNotesStep(page, contactId)

    // Add C in the form; the methods step lands, notes fails, form stays open.
    await page.getByRole('button', { name: 'Add method' }).click()
    await page.getByRole('combobox', { name: 'Contact method type' }).nth(1).selectOption('phone')
    await methodValues(page).nth(1).fill(phoneC)
    await save(page)
    await expect(page.getByTestId('save-report')).toBeVisible({ timeout: 15000 })

    const afterFirst = await getMethods(request, contactId)
    expect(afterFirst.map(m => m.value).sort()).toEqual([emailA, '5555550145', phoneC].sort())

    // Now delete C and save again. The client learned C's id from `results`.
    const cIndex = await methodValues(page).evaluateAll(
      (inputs, target) => inputs.findIndex(i => (i as HTMLInputElement).value === target),
      phoneC
    )
    expect(
      cIndex,
      'the row added in the failed save should still be on screen'
    ).toBeGreaterThanOrEqual(0)
    await page.getByRole('button', { name: 'Remove' }).nth(cIndex).click()
    await saveAndAwait(page, contactId, 'methods')

    const methods = await getMethods(request, contactId)
    const values = methods.map(m => m.value)
    expect(values).not.toContain(phoneC)
    // And the unseen B survived: the acknowledged state absorbed only the rows
    // this client's own operations resolved to.
    expect(values.sort()).toEqual([emailA, '5555550145'].sort())
  })

  test('a method added in a partially failed save can then be edited, keeping its identity', async ({
    page,
    request,
  }) => {
    // spec: CON-063.partial-save-add-can-be-edited
    const phoneC = '5555550146'
    const phoneCEdited = '5555550147'
    const seeded = await testApi.seedBehavior('CON-063')
    const contactId = seeded.entities['target'].id

    await openEditMode(page, contactId)
    await failNotesStep(page, contactId)

    await page.getByRole('button', { name: 'Add method' }).click()
    await page.getByRole('combobox', { name: 'Contact method type' }).nth(1).selectOption('phone')
    await methodValues(page).nth(1).fill(phoneC)
    await save(page)
    await expect(page.getByTestId('save-report')).toBeVisible({ timeout: 15000 })

    const created = (await getMethods(request, contactId)).find(m => m.value === phoneC)
    expect(created, 'the added method should exist after the methods step landed').toBeTruthy()

    const cIndex = await methodValues(page).evaluateAll(
      (inputs, target) => inputs.findIndex(i => (i as HTMLInputElement).value === target),
      phoneC
    )
    expect(
      cIndex,
      'the row added in the failed save should still be on screen'
    ).toBeGreaterThanOrEqual(0)
    await methodValues(page).nth(cIndex).fill(phoneCEdited)
    await saveAndAwait(page, contactId, 'methods')

    const edited = (await getMethods(request, contactId)).find(m => m.value === phoneCEdited)
    expect(edited, 'the edit should have landed').toBeTruthy()
    // Identity preserved. Skipping the results write-back turns this into a
    // remove + add and mints a new id — a mild instance of the incident's own
    // signature, and it would destroy the forensic evidence a future
    // investigation would need.
    expect(edited!.id).toBe(created!.id)
  })

  test('reverting a value after a partially failed save restores it on the server', async ({
    page,
    request,
  }) => {
    // spec: CON-063.reverting-method-value-restores
    const emailIntermediate = `revert-b-${Date.now()}@example.com`
    const seeded = await testApi.seedBehavior('CON-063')
    const contactId = seeded.entities['target'].id
    const emailA = await seededEmail(request, contactId)

    await openEditMode(page, contactId)
    await failNotesStep(page, contactId)

    await methodValues(page).first().fill(emailIntermediate)
    await save(page)
    await expect(page.getByTestId('save-report')).toBeVisible({ timeout: 15000 })
    expect((await getMethods(request, contactId)).map(m => m.value)).toEqual([emailIntermediate])

    // Revert. Against a frozen edit-start baseline this emits nothing, the save
    // reports success, and the server silently keeps the intermediate value.
    await methodValues(page).first().fill(emailA)
    await saveAndAwait(page, contactId, 'methods')

    expect((await getMethods(request, contactId)).map(m => m.value)).toEqual([emailA])
  })
})
