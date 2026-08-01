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
    // CAL-019.past-confirmed-events-additionally (past-projects/future-links split) and CAL-019.append-does-not-reset (processed
    // flag not reset) are waived in spec/calendar.yaml: both are backend
    // projection plumbing owned by rematch_integration_test.go.
    // spec: CAL-019.contact-appended-each-matching
    //
    // The declared world holds a method-less contact plus a past meeting the real
    // calendar sync stored with an UNMATCHED attendee: the contact is absent from
    // matched_contact_ids, so the event is absent from GET /contacts/:id/events
    // until rematch links it. The manifest reports the attendee's address as the
    // event's name — the generator owns it, and it is the value this flow types
    // into the edit form.
    const seeded = await testApi.seedBehavior('CAL-019')
    const contactId = seeded.entities['target'].id
    const seededEventId = seeded.entities['stored'].id
    const attendeeEmail = seeded.entities['stored'].name

    // Navigate straight into the edit view via the existing ?action=edit query
    // param (same path the list-page "Edit" context menu uses).
    await page.goto(`/contacts/${contactId}?action=edit`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: 'Edit Contact' })).toBeVisible({
      timeout: 15000,
    })

    // Add an email method. The form starts with one empty method row (type
    // "Select", value ""); pick email + fill the attendee address via the
    // rows' accessible names.
    await page.getByRole('combobox', { name: 'Contact method type' }).first().selectOption('email')
    // The chosen method type drives the value input's type attribute (email
    // keyboard + native validation) — pin the dynamic inputType wiring, which
    // the accessible-name locator alone would not observe.
    const valueInput = page.getByRole('textbox', { name: 'Contact method value' }).first()
    await expect(valueInput).toHaveAttribute('type', 'email')
    await valueInput.fill(attendeeEmail)

    // The rematch job is minted by the operations endpoint, not the contact
    // PUT: a rematch fires on newly-present method VALUES, and the scalar PUT
    // no longer carries methods at all.
    const methodsPath = `/api/v1/contacts/${contactId}/methods`
    const methodsResponsePromise = page.waitForResponse(res => {
      const { pathname } = new URL(res.url())
      return pathname === methodsPath && res.request().method() === 'POST' && res.status() === 200
    })
    await page.getByRole('button', { name: 'Update Contact' }).click()
    const methodsResponse = await methodsResponsePromise
    expect(methodsResponse.ok()).toBe(true)
    const methodsBody = await methodsResponse.json()
    const rematchJobId: string | undefined = methodsBody?.data?.rematch_job_id
    expect(
      rematchJobId,
      'POST /contacts/:id/methods response should carry rematch_job_id'
    ).toBeTruthy()

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
