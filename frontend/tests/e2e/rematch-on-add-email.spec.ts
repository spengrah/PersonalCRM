import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// Exercise the full rematch happy path: seed an unmatched calendar event
// with an attendee email, add that email to a CRM contact, and assert — via
// the calendar API, not the DOM — that the event is linked to the contact once
// the rematch job completes. The frontend polls the job silently
// (RematchJobsProvider); this test polls the backend directly. @area:contacts
// spec: IMP-021
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
    // spec: CAL-019
    const attendeeEmail = `rematch-${Date.now()}@example.com`

    // Seed a contact with no email so the rematch handler has something to link to.
    const { ids } = await testApi.seedContacts([{ full_name: 'Rematch Target' }])
    const contactId = ids[0]

    // Seed an unmatched past calendar event with the attendee email. The
    // `unmatched` flag keeps contact_id OUT of matched_contact_ids so the
    // event stays unlinked (absent from GET /contacts/:id/events) until rematch
    // links it.
    const { ids: eventIds } = await testApi.seedCalendarEvents(contactId, [
      {
        title: 'Rematch Meeting',
        is_past: true,
        days_ago: 3,
        attendee_emails: [attendeeEmail],
        unmatched: true,
      },
    ])
    const seededEventId = eventIds[0]

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

    // The edit form also saves notes via PUT /contacts/:id/notes in parallel,
    // so match the exact contact update route before reading rematch_job_id.
    const contactUpdatePath = `/api/v1/contacts/${contactId}`
    const updateResponsePromise = page.waitForResponse(res => {
      const { pathname } = new URL(res.url())
      return (
        pathname === contactUpdatePath && res.request().method() === 'PUT' && res.status() === 200
      )
    })
    await page.getByRole('button', { name: 'Update Contact' }).click()
    const updateResponse = await updateResponsePromise
    expect(updateResponse.ok()).toBe(true)
    const updateBody = await updateResponse.json()
    const rematchJobId: string | undefined = updateBody?.data?.rematch_job_id
    expect(rematchJobId, 'PUT /contacts response should carry rematch_job_id').toBeTruthy()

    // The frontend polls the job silently; poll the backend directly here so
    // the test observes the pollable job endpoint (IMP-021) and fails fast.
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

    // Assert the data outcome via the calendar API, not the DOM: the previously
    // unmatched event is now linked to the contact, so it appears in
    // GET /contacts/:id/events (which returns only events matched to the
    // contact). This is CAL-019's observable — the contact is appended to the
    // matching event's matched set.
    let eventLinked = false
    for (let i = 0; i < 20 && !eventLinked; i++) {
      const res = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}/events`, {
        headers: API_HEADERS,
      })
      if (res.ok()) {
        const body = await res.json()
        const events: Array<{ id: string }> = body?.data ?? []
        if (events.some(e => e.id === seededEventId)) {
          eventLinked = true
          break
        }
      }
      await new Promise(r => setTimeout(r, 250))
    }
    expect(eventLinked, 'rematched event should be linked to the contact').toBe(true)
  })
})
