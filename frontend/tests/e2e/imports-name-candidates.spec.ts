import { test, expect } from './fixtures'
import {
  createTestAPI,
  declaredWorldNamePrefix,
  TestAPI,
  type SeedBehaviorResult,
} from './helpers/test-api'

// People-tab name candidates: grouped anarlog_title tokens lifted from session
// titles. Each (token, session) pair is one external_contact row; the Imports
// UI groups them by normalized token and resolves the whole group in one call.
//
// Every test here rides IMP-026's declared queue, which carries two
// anarlog_title rows under ONE namespace-prefixed token — so they group into a
// single row whose evidence count is two, and no sibling worker's token can join
// the group (the grouping is DB-wide and has no namespace scoping).
test.describe('Imports name candidates (anarlog_title) @area:imports', () => {
  let testApi: TestAPI
  let seeded: SeedBehaviorResult
  // The grouped row's display token, as the discovery writer stored it (it
  // title-cases what it is given), and the normalized token the row's test id
  // and the resolve request body both key on.
  let display: string
  let token: string

  test.beforeEach(async ({ request }, testInfo) => {
    // IMP-026's queue includes an ingest-pipeline candidate, which settles a
    // River cascade — hence the wider budget.
    test.setTimeout(60_000)
    testApi = createTestAPI(request, testInfo)
    seeded = await testApi.seedBehavior('IMP-026')
    display = seeded.entities['token-a'].name
    // The writer's own invariant: token_normalized is the lower-cased display
    // token. Both declared rows share it, which is what makes them one group.
    token = display.toLowerCase()
    expect(seeded.entities['token-b'].name).toBe(display)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-026.low-confidence-names-section
  test('renders the name-candidate section with the grouped token and evidence count', async ({
    page,
  }) => {
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    // The section appears when title-derived tokens exist; the heading is
    // the region's accessible locator.
    await expect(page.getByRole('heading', { name: 'Names found in session titles' })).toBeVisible({
      timeout: 10000,
    })
    await expect(page.getByText(display, { exact: true }).first()).toBeVisible()
    // Evidence count reflects the two declared (token, session) rows. The
    // literal MIRRORS the declaration's own two token entities rather than a
    // generated value.
    await expect(page.getByText(/Seen in 2 session titles/).first()).toBeVisible()
  })

  // Scope to THIS test's exact name-candidate row by its token test id. A
  // hasText filter matches ancestor containers that wrap OTHER workers' rows
  // too, so `.first()` there could open the modal on a sibling worker's
  // candidate; the test id pins the precise row element.
  const myRow = (page: import('@playwright/test').Page) =>
    page.getByTestId(`name-candidate-row-${token}`)

  // Match only THIS worker's resolve POST (the body carries normalized_token).
  // Without scoping, parallel workers' resolve responses cross-match and a
  // sibling worker's response can be awaited here.
  const myResolve = (page: import('@playwright/test').Page) =>
    page.waitForResponse(res => {
      if (
        res.request().method() !== 'POST' ||
        !res.url().includes('/api/v1/imports/anarlog-title/resolve')
      ) {
        return false
      }
      try {
        return (
          (res.request().postDataJSON() as { normalized_token?: string }).normalized_token === token
        )
      } catch {
        return false
      }
    })

  // spec: IMP-031.item-leaves-queue-counts-update
  test('imports the whole token group as a new contact', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(display, { exact: true }).first()).toBeVisible({ timeout: 10000 })

    await myRow(page)
      .getByRole('button', { name: /Create contact/ })
      .first()
      .click()

    // The modal opens in the name-only branch: no contact-methods section,
    // just name + cadence + the info note. The pager opens at this token.
    const dialog = page.getByRole('dialog', { name: /Create contact from name candidate/ })
    await expect(dialog.getByText(/No contact methods/)).toBeVisible()
    await expect(dialog.getByLabel('Name')).toHaveValue(display)

    const resolved = myResolve(page)
    await dialog.getByRole('button', { name: 'Create contact', exact: true }).click()
    const res = await resolved
    expect(res.ok()).toBeTruthy()

    // The token group leaves the name-candidate list after resolution.
    await expect(page.getByText(display, { exact: true })).toHaveCount(0, { timeout: 10000 })
  })

  // spec: IMP-031.item-leaves-queue-counts-update
  test('links the token group to an existing contact', async ({ page }) => {
    // The link target is IMP-026's own declared contact, so it lives in the same
    // namespace as the token group and the selector's substring filter reaches it
    // — once that filter is actually applied, which the selection below does.
    const targetName = seeded.entities['link-target'].name

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(display, { exact: true }).first()).toBeVisible({ timeout: 10000 })

    await myRow(page)
      .getByRole('button', { name: /Create contact/ })
      .first()
      .click()
    const dialog = page.getByRole('dialog', { name: /Create contact from name candidate/ })
    await expect(dialog.getByText(/No contact methods/)).toBeVisible()

    // Toggle to link mode and pick the declared contact. TYPE THE WORLD'S NAME
    // PREFIX FIRST: ContactSelector renders `contacts.slice(0, 10)` after a
    // substring filter that an empty term matches everything with
    // (contact-selector.tsx:53-67), and its list is an unscoped
    // useContacts({ limit: 500 }) (NameCandidateModal.tsx:60) — so on the shared
    // parallel database the ten rendered options are whatever ten names sort
    // first across every worker, and this world's target need not be among them.
    // Filtering to `synth-<namespace>-` leaves only this world's contacts, the
    // same pattern every sibling link flow uses.
    await dialog.getByRole('button', { name: 'Link to existing' }).click()
    await dialog.getByText('Search contacts...').click()
    await dialog.getByPlaceholder('Search contacts...').fill(declaredWorldNamePrefix(seeded))
    const targetOption = dialog.getByText(targetName, { exact: true }).last()
    await expect(targetOption).toBeVisible({ timeout: 5000 })
    await targetOption.click()

    const resolved = myResolve(page)
    await dialog.getByRole('button', { name: 'Link contact', exact: true }).click()
    const res = await resolved
    expect(res.ok()).toBeTruthy()

    await expect(page.getByText(display, { exact: true })).toHaveCount(0, { timeout: 10000 })
  })

  // spec: IMP-031.item-leaves-queue-counts-update
  test('ignores the token group via "Not a person"', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(display, { exact: true }).first()).toBeVisible({ timeout: 10000 })

    const resolved = myResolve(page)
    // The row-level "Not a person" ignores the group directly (no modal).
    await myRow(page)
      .getByRole('button', { name: /Not a person/ })
      .first()
      .click()
    const res = await resolved
    expect(res.ok()).toBeTruthy()

    await expect(page.getByText(display, { exact: true })).toHaveCount(0, { timeout: 10000 })
  })
})
