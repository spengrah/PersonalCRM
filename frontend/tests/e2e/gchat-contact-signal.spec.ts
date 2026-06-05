import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

// Realistic E2E scope for the GChat contact surface (PR 3, Q2-A). The contact
// detail page renders no per-interaction timeline (it shows only direction
// signals), and the public interactions POST API only creates source='manual'
// interactions — so a gchat-source line item cannot be seeded through the UI or
// public API. We therefore assert:
//   - the direction-signal behavior on the detail page (mirroring
//     contact-direction.spec.ts), which is what a gchat-derived interaction
//     bumps; and
//   - the interactions-API response SHAPE that a real gchat interaction surfaces
//     through (description + direction + source), using a description of the
//     "GChat … (N messages)" form to mirror what the integration path produces.
// The full enable→sweep→aggregate→cadence chain (where the real gchat
// interaction is produced) lives in the backend Phase-3 integration test.

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

test.describe('GChat Contact Signals @area:contacts', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('contact detail page shows direction signals after a mutual interaction', async ({
    page,
    request,
  }) => {
    const { ids } = await testApi.seedContacts([
      { full_name: 'GChat Signal Test', cadence: 'monthly' },
    ])
    const contactId = ids[0]

    // A gchat mutual exchange bumps both direction signals; record an
    // equivalent interaction via the public API (which is manual-source) to
    // exercise the same detail-page rendering path.
    const pastDate = new Date(Date.now() - 2 * 24 * 60 * 60 * 1000).toISOString()
    await request.post(`${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`, {
      headers: API_HEADERS,
      data: { occurred_at: pastDate, direction: 'mutual' },
    })

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'GChat Signal Test' })).toBeVisible({
      timeout: 15000,
    })

    await expect(page.getByText('Last outreach:')).toBeVisible({ timeout: 5000 })
    await expect(page.getByText('Last response:')).toBeVisible()
  })

  test('interactions API surfaces description + direction + source (gchat description shape)', async ({
    request,
  }) => {
    const { ids } = await testApi.seedContacts([{ full_name: 'GChat Interaction Shape Test' }])
    const contactId = ids[0]

    // The "GChat <label> (<n> messages)" description is what the aggregation
    // engine writes for a real gchat interaction. The public interactions API
    // is manual-source, so we assert the response SHAPE that carries that
    // description form end-to-end (description + direction), not a rendered
    // line item (infeasible — the detail page has no interaction timeline).
    const gchatDescription = 'GChat exchange (3 messages)'
    const pastDate = new Date(Date.now() - 60 * 60 * 1000).toISOString()
    const createResp = await request.post(
      `${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`,
      {
        headers: API_HEADERS,
        data: { occurred_at: pastDate, direction: 'inbound', description: gchatDescription },
      }
    )
    expect(createResp.ok()).toBeTruthy()
    const createBody = await createResp.json()
    expect(createBody.data.direction).toBe('inbound')
    expect(createBody.data.description).toBe(gchatDescription)

    const listResp = await request.get(
      `${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`,
      { headers: API_HEADERS }
    )
    expect(listResp.ok()).toBeTruthy()
    const listBody = await listResp.json()
    expect(listBody.data.length).toBeGreaterThan(0)

    const row = listBody.data[0]
    expect(row).toHaveProperty('description')
    expect(row).toHaveProperty('direction')
    expect(row).toHaveProperty('source')
    expect(row.description).toContain('GChat')
    expect(row.description).toContain('messages')
    expect(row.direction).toBe('inbound')
  })
})
