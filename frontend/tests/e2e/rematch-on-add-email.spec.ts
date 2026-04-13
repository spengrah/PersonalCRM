import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// Exercise the full rematch happy path: seed an unmatched calendar event
// with an attendee email, add that email to a CRM contact, and assert the
// Meetings tab shows the event once the rematch job completes. The frontend
// polls the job silently (RematchJobsProvider); invalidation causes the
// Meetings list to refresh. @area:contacts
test.describe('Rematch on add email @area:contacts', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('adding an email retroactively links a past calendar event to the contact', async ({
    page,
    request,
  }) => {
    const attendeeEmail = `rematch-${Date.now()}@example.com`

    // Seed a contact with no email so the rematch handler has something to link to.
    const { ids } = await testApi.seedContacts([{ full_name: 'Rematch Target' }])
    const contactId = ids[0]

    // Seed an unmatched past calendar event with the attendee email. The
    // `unmatched` flag keeps contact_id OUT of matched_contact_ids so the
    // event isn't visible on the Meetings tab until rematch links it.
    await testApi.seedCalendarEvents(contactId, [
      {
        title: 'Rematch Meeting',
        is_past: true,
        days_ago: 3,
        attendee_emails: [attendeeEmail],
        unmatched: true,
      },
    ])

    // Navigate straight into the edit view via the existing ?action=edit query
    // param (same path the list-page "Edit" context menu uses).
    await page.goto(`/contacts/${contactId}?action=edit`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: 'Edit Contact' })).toBeVisible({
      timeout: 15000,
    })

    // Add an email method. The form starts with one empty method row (type
    // "Select", value ""); pick email + fill the attendee address.
    const firstRow = page.locator('.group').first()
    await firstRow.getByRole('combobox').selectOption('email')
    await firstRow.locator('input[type="email"]').fill(attendeeEmail)

    // Capture the PUT response so we can read back the rematch_job_id.
    const updateResponsePromise = page.waitForResponse(
      res => res.url().includes(`/api/v1/contacts/${contactId}`) && res.request().method() === 'PUT'
    )
    await page.getByRole('button', { name: 'Update Contact' }).click()
    const updateResponse = await updateResponsePromise
    expect(updateResponse.ok()).toBe(true)
    const updateBody = await updateResponse.json()
    const rematchJobId: string | undefined = updateBody?.data?.rematch_job_id
    expect(rematchJobId, 'PUT /contacts response should carry rematch_job_id').toBeTruthy()

    // The frontend polls the job silently. Poll the backend directly from the
    // test so we can fail fast if something is wrong — visual assertions below
    // cover the actual user-visible outcome (Meetings tab refresh).
    let jobCompleted = false
    for (let i = 0; i < 40 && !jobCompleted; i++) {
      const res = await request.get(`${API_BASE_URL}/api/v1/rematch/jobs/${rematchJobId}`, {
        headers: API_HEADERS,
      })
      if (res.ok()) {
        const body = await res.json()
        if (body?.data?.status === 'completed' || body?.data?.status === 'failed') {
          expect(body.data.status).toBe('completed')
          expect(body.data.matched).toBeGreaterThanOrEqual(1)
          jobCompleted = true
          break
        }
      }
      await new Promise(r => setTimeout(r, 250))
    }
    expect(jobCompleted, 'rematch job should reach a terminal state').toBe(true)

    // The provider's invalidation should refresh the Meetings list. Click the
    // All tab so past events are visible regardless of default filter.
    await page.getByRole('button', { name: /All \(\d+\)/i }).click()

    await expect(page.getByText(`${testApi.prefix}-Rematch Meeting`)).toBeVisible({
      timeout: 10000,
    })
  })
})
