import { test, expect } from './fixtures'
import type { APIRequestContext } from '@playwright/test'
import {
  createTestAPI,
  TestAPI,
  declaredWorldNamePrefix,
  type SeedBehaviorResult,
} from './helpers/test-api'
import {
  expectModalCandidate,
  findCandidateByName,
  candidateCardByName,
} from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

/**
 * The '@handle' a declared telegram candidate actually carries, read back from
 * the candidate endpoint. The generator owns the handle, so a test that rebuilt
 * it from the namespace would be asserting against a string it invented.
 */
async function telegramHandleOf(request: APIRequestContext, candidateId: string): Promise<string> {
  const res = await request.get(`${API_BASE_URL}/api/v1/imports/${candidateId}`, {
    headers: API_HEADERS,
  })
  expect(res.ok()).toBe(true)
  const handle: string = (await res.json())?.data?.metadata?.username
  expect(handle, 'the declared telegram candidate must carry a handle').toBeTruthy()
  return handle
}

test.describe('Imports Features @area:imports', () => {
  test.describe('Pagination', () => {
    let testApi: TestAPI
    let seeded: SeedBehaviorResult

    test.beforeEach(async ({ request }, testInfo) => {
      // IMP-026's queue is a pageful-and-one of candidates plus an
      // ingest-pipeline one that settles a River cascade.
      test.setTimeout(60_000)
      testApi = createTestAPI(request, testInfo)
      seeded = await testApi.seedBehavior('IMP-026')
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-026.people-tab-default-holds
    test('paginates the candidate list', async ({ page, request }) => {
      // Positive, namespace-scoped anchor FIRST. The page-2 assertion below is
      // one-sided and unscoped, so on its own another worker's rows satisfy it
      // even if this fixture produced nothing at all. The expected id set comes
      // from the manifest (the declaration numbers its queue handles p01..pNN),
      // so nothing here restates a count.
      const declaredQueueIDs = Object.entries(seeded.entities)
        .filter(([handle]) => /^p\d\d$/.test(handle))
        .map(([, entity]) => entity.id)
      expect(declaredQueueIDs.length).toBeGreaterThan(20)
      const queueRes = await request.get(`${API_BASE_URL}/api/v1/imports/candidates?limit=10000`, {
        headers: API_HEADERS,
      })
      expect(queueRes.ok()).toBe(true)
      const queued: Array<{ id: string }> = (await queueRes.json())?.data ?? []
      const queuedIDs = new Set(queued.map(c => c.id))
      expect(declaredQueueIDs.filter(id => !queuedIDs.has(id))).toEqual([])

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Pagination controls render once the queue exceeds a page.
      const nextButton = page.getByRole('button', { name: 'Next', exact: true })
      await expect(page.getByRole('button', { name: 'Previous', exact: true })).toBeVisible()
      await expect(nextButton).toBeVisible()

      // Clicking Next refetches the suggestions feed with the page-2 param
      // and the second page is non-empty (the declared queue alone exceeds
      // one page of 20).
      const page2Response = page.waitForResponse(
        res =>
          res.request().method() === 'GET' &&
          res.url().includes('/api/v1/imports/suggestions') &&
          res.url().includes('page=2')
      )
      await nextButton.click()
      const res = await page2Response
      expect(res.ok()).toBe(true)
      const body = await res.json()
      expect((body?.data ?? []).length).toBeGreaterThan(0)

      // Route/UI state reflects being past the first page.
      await expect(page.getByRole('button', { name: 'Previous', exact: true })).toBeEnabled()
    })
  })

  test.describe('Suggested Matches', () => {
    // These tests verify the suggested-match affordance against IMP-029's
    // declared confidence ladder.

    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-029.without-suggestion-link-action
    test('should show "Link (select)" when no suggested match', async ({ page }) => {
      // IMP-029's unmatched address-book candidate: its generated name and email
      // are unrelated to every contact in the world, so nothing reaches the
      // suggestion confidence threshold.
      const seeded = await testApi.seedBehavior('IMP-029')
      const displayName = seeded.entities['unmatched-gc'].name

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      await findCandidateByName(page, displayName)

      // The candidate shows "Link (select)" since no CRM contact matches it.
      const candidateCard = candidateCardByName(page, displayName)
      await expect(candidateCard.getByRole('button', { name: 'Link (select)' })).toBeVisible()
    })

    // spec: IMP-029.link-action-names-suggested
    test('should show suggested match with confidence percentage when present', async ({
      page,
    }) => {
      // IMP-029's high-confidence pair: the candidate shares the contact's name
      // AND its primary email, which is what the fuzzy matcher scores highest.
      const seeded = await testApi.seedBehavior('IMP-029')
      const displayName = seeded.entities['matching'].name

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      await findCandidateByName(page, displayName)

      // The link action names the suggested contact with its confidence
      // percentage: "Link to [Name] (XX%)".
      const candidateCard = candidateCardByName(page, displayName)
      const linkButton = candidateCard.getByRole('button', { name: /Link to/ })
      await expect(linkButton).toBeVisible()
      await expect(linkButton).toContainText(displayName)
      await expect(linkButton).toContainText('%')
    })

    // spec: IMP-028.suggested-match-when-present
    test('should pre-select suggested contact in link modal', async ({ page }) => {
      // Rides IMP-029's high-confidence pair (IMP-028's own declaration is the
      // two-row pager queue, which carries no suggested match at all).
      const seeded = await testApi.seedBehavior('IMP-029')
      const displayName = seeded.entities['matching'].name

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      await findCandidateByName(page, displayName)

      // Click the Link button (which should show "Link to [Name] (XX%)")
      await candidateCardByName(page, displayName)
        .getByRole('button', { name: /Link to/ })
        .click()

      // Verify modal opens with mode toggle
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

      await expectModalCandidate(page, displayName)

      // The suggested contact is pre-selected: the ContactSelector shows the
      // selection (no search placeholder) and the Link action is enabled
      // (it is disabled when no contact is selected).
      const dialog = page.getByRole('dialog', { name: 'Resolve import candidate' })
      await expect(dialog.getByText('Search for a contact...')).toHaveCount(0)
      await expect(page.getByRole('button', { name: /Link Contact/i })).toBeEnabled()
    })
  })

  test.describe('Telegram @username display', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-036.username-appears-chip-deep
    test('shows @username chip on Telegram candidate card', async ({ page, request }) => {
      const seeded = await testApi.seedBehavior('IMP-036')
      const displayName = seeded.entities['named'].name
      const handle = await telegramHandleOf(request, seeded.entities['named'].id)
      // The chip's href is built the same way the component builds it, so the
      // assertion cannot pass on a differently-escaped path.
      const telegramPath = encodeURIComponent(handle.replace(/^@/, ''))

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Filter to Telegram so only telegram cards are visible even on a busy DB.
      await page.getByRole('button', { name: 'Telegram', exact: true }).click()

      await findCandidateByName(page, displayName)

      // Heading shows the display_name; chip shows the handle and links to t.me
      await expect(page.getByRole('heading', { name: displayName })).toBeVisible()
      const handleLink = page.getByRole('link', { name: handle })
      await expect(handleLink).toBeVisible()
      await expect(handleLink).toHaveAttribute('href', `https://t.me/${telegramPath}`)
    })

    // spec: IMP-036.no-name-fields-username-used
    test('falls back to @username when no name is set on Telegram candidate', async ({
      page,
      request,
    }) => {
      // The handle-only peer: the discovery pass learned a handle but no name
      // fields, so the manifest reports an empty stored display name.
      const seeded = await testApi.seedBehavior('IMP-036')
      expect(seeded.entities['handle-only'].name).toBe('')
      const handle = await telegramHandleOf(request, seeded.entities['handle-only'].id)

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      await page.getByRole('button', { name: 'Telegram', exact: true }).click()

      // @username becomes the primary heading
      await expect(page.getByRole('heading', { name: handle })).toBeVisible()
      // Chip is suppressed when the handle is already the heading. Scoping
      // by the namespace-unique handle avoids clashing with other seeded
      // rows on a shared DB.
      const handleLinks = page.getByRole('link', { name: handle })
      await expect(handleLinks).toHaveCount(0)
    })

    // A candidate with no source name fields must still be importable without
    // editing the name — the frontend sends the @handle as `name` explicitly,
    // because the backend can't derive it from display_name/first_name/last_name.
    // spec: IMP-012.crm-contact-created-normal-path, IMP-012.methods-come-from-selection
    test('imports a handle-only Telegram candidate without requiring a name edit', async ({
      page,
      request,
    }) => {
      const seeded = await testApi.seedBehavior('IMP-012')
      const handle = await telegramHandleOf(request, seeded.entities['handle-only'].id)

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')
      await page.getByRole('button', { name: 'Telegram', exact: true }).click()

      // The candidate card heading shows the handle (display-name fallback).
      await expect(page.getByRole('heading', { name: handle })).toBeVisible()

      // Open the Import action for this candidate.
      await candidateCardByName(page, handle)
        .getByRole('button', { name: /Import/i })
        .click()

      // Modal opens in import mode (mode toggle is visible).
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      await expectModalCandidate(page, handle)

      // Contact Methods section shows the @handle as a selectable method row —
      // NOT "No contact methods available". The row carries the handle as its
      // visible value and defaults to selected.
      const dialog = page.getByRole('dialog', { name: 'Resolve import candidate' })
      await expect(dialog.getByText('No contact methods available')).not.toBeVisible()
      const methodRow = dialog.locator('div.border', { hasText: handle }).last()
      await expect(methodRow.getByRole('button', { name: 'Deselect method' })).toHaveAttribute(
        'aria-pressed',
        'true'
      )

      // Import without editing the name; the created contact carries the
      // handle as its name and the telegram method from the external record.
      const importResponsePromise = page.waitForResponse(
        res => res.request().method() === 'POST' && /\/imports\/.+\/import$/.test(res.url())
      )
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

      const importResponse = await importResponsePromise
      expect(importResponse.status()).toBe(201)
      const importBody = await importResponse.json()
      const createdContactId: string = importBody?.data?.contact?.id
      expect(createdContactId).toBeTruthy()

      const contactRes = await request.get(`${API_BASE_URL}/api/v1/contacts/${createdContactId}`, {
        headers: API_HEADERS,
      })
      expect(contactRes.ok()).toBe(true)
      const contactBody = await contactRes.json()
      expect(contactBody?.data?.full_name).toBe(handle)
      const methods: Array<{ type: string; value: string }> = contactBody?.data?.methods ?? []
      expect(methods.some(m => m.type === 'telegram')).toBe(true)
    })

    // spec: IMP-027.methods-selectable-one-primary
    test('shows @username method in Link to Existing modal', async ({ page, request }) => {
      // Rides IMP-027's resolver fixture: it carries both a handle-only telegram
      // peer and cadence-bearing contacts to link one to.
      const seeded = await testApi.seedBehavior('IMP-027')
      const handle = await telegramHandleOf(request, seeded.entities['tg-handle'].id)
      const targetName = seeded.entities['cadenced'].name

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')
      await page.getByRole('button', { name: 'Telegram', exact: true }).click()

      await candidateCardByName(page, handle).getByRole('button', { name: /Link/i }).click()

      await expectModalCandidate(page, handle)

      // Switch to Link mode
      await page.getByRole('button', { name: 'Link to Existing', exact: true }).click()

      // Select the CRM contact
      const dialog = page.getByRole('dialog', { name: 'Resolve import candidate' })
      await dialog.getByText('Search for a contact...').click()
      await dialog.getByPlaceholder('Search for a contact...').fill(declaredWorldNamePrefix(seeded))
      await page.getByText(targetName, { exact: true }).last().click()

      // Link mode groups the handle under the to-add bucket ("Will be
      // added") rather than "No contact methods available".
      await expect(dialog.getByText('No contact methods available')).not.toBeVisible()
      await expect(dialog.getByText('Will be added')).toBeVisible()
      const methodRow = dialog.locator('div.border', { hasText: handle }).last()
      await expect(
        methodRow.getByRole('button', { name: /Select method|Deselect method/ })
      ).toBeVisible()
    })
  })
})
