import { test, expect } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import { API_BASE_URL, API_HEADERS, createTestAPI, TestAPI } from './helpers/test-api'
import { fetchTimeline, fetchTimelineContent, itemWithSource } from './helpers/timeline-items'

test.describe('Interaction assertions beside evidence @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IXN-023.assertion-beside-cited-content, IXN-023.read-only-no-review-controls
  test('shows a claim citing a message beside that message, read-only', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-023')
    const contactId = seeded.entities.subject.id
    const claim = seeded.entities.claim
    const item = itemWithSource((await fetchTimeline(request, contactId)).items, 'email')
    const messages = (
      await fetchTimelineContent(request, item.identity, contactId)
    ).members.flatMap(member => member.messages)
    expect(messages, 'the seeded email interaction displays one message').toHaveLength(1)
    const contact = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
      headers: API_HEADERS,
    })
    expect(contact.ok()).toBe(true)
    const subjectName = (await contact.json()).data.full_name as string

    await page.goto(`/contacts/${contactId}`)
    const row = page
      .getByRole('region', { name: 'Interactions' })
      .locator(`[data-interaction-id="${item.identity.id}"]`)
    await row.getByRole('button', { name: 'Expand content', exact: true }).click()

    // The claim appears inside the message it cites, stating what it claims
    // and its recorded status.
    const message = row.locator(`[data-message-id="${messages[0].id}"]`)
    const cited = message.getByRole('list', { name: 'Assertions citing this content' })
    await expect(cited).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    const entry = cited.locator(`[data-assertion-id="${claim.id}"]`)
    await expect(entry.locator('[data-assertion-subject]')).toHaveText(subjectName)
    await expect(entry.locator('[data-assertion-predicate]')).toHaveText('preference')
    await expect(entry.locator('[data-assertion-value]')).toHaveText(claim.name)
    await expect(entry.locator('[data-assertion-status]')).toHaveText('Proposed')

    // Read-only: no control to accept or reject the claim, no link into the
    // graph, and no baseline or verdict beside it.
    await expect(cited.getByRole('listitem')).toHaveCount(1)
    await expect(cited).not.toContainText(/baseline|verdict/i)
    await expect(cited.locator('button, a, input, select, textarea')).toHaveCount(0)
  })
})
