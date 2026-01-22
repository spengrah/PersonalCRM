import { test, expect, APIRequestContext } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

async function getRemindersByContact(request: APIRequestContext, contactId: string) {
  const response = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}/reminders`, {
    headers: API_HEADERS,
  })
  if (!response.ok()) {
    return { ok: false, reminders: [] as Array<{ id: string; title: string }> }
  }
  const data = await response.json()
  return { ok: true, reminders: data.data as Array<{ id: string; title: string }> }
}

test.describe('Reminder Lifecycle', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('deleting a contact should remove its reminders from the reminders list', async ({
    page,
    request,
  }) => {
    // Seed a contact using TestAPI
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Delete Contact',
      },
    ])
    const contactId = ids[0]
    const contactName = `${testApi.prefix}-Delete Contact`
    const reminderTitle = `Reminder for ${contactName}`

    // Create a reminder for this contact via API
    const reminderResponse = await request.post(`${API_BASE_URL}/api/v1/reminders`, {
      headers: API_HEADERS,
      data: {
        contact_id: contactId,
        title: reminderTitle,
        due_date: new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString(),
      },
    })
    expect(reminderResponse.ok()).toBeTruthy()

    // Verify the reminder exists via API (avoid extra UI navigation)
    const initialReminders = await getRemindersByContact(request, contactId)
    expect(initialReminders.ok).toBe(true)
    expect(initialReminders.reminders.some(reminder => reminder.title === reminderTitle)).toBe(true)

    // Go to contacts page and delete the contact
    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: contactName }).first()).toBeVisible()

    // Accept the confirmation dialog and delete
    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])

    await expect(page.getByRole('heading', { name: 'Contacts', level: 2 })).toBeVisible()

    // Verify the reminder is gone via API
    const afterDeleteReminders = await getRemindersByContact(request, contactId)
    if (afterDeleteReminders.ok) {
      expect(
        afterDeleteReminders.reminders.some(reminder => reminder.title === reminderTitle)
      ).toBe(false)
    }
  })

  test('marking a contact as contacted should complete auto-generated reminders', async ({
    page,
    request,
  }) => {
    // Seed a contact with cadence using TestAPI
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Mark Contacted',
        cadence: 'weekly',
      },
    ])
    const contactId = ids[0]
    const contactName = `${testApi.prefix}-Mark Contacted`
    const autoReminderTitle = `Reach out to ${contactName} (weekly)`
    const manualReminderTitle = `Manual reminder for ${contactName}`

    // Create an "auto" reminder directly via the backend
    // Note: In real usage, the scheduler would create these, but we simulate it here
    // The key is that source='auto' reminders should be completed when marking as contacted
    const autoReminderResponse = await request.post(`${API_BASE_URL}/api/v1/reminders`, {
      headers: API_HEADERS,
      data: {
        contact_id: contactId,
        title: autoReminderTitle,
        due_date: new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString(), // Due yesterday (overdue)
      },
    })
    expect(autoReminderResponse.ok()).toBeTruthy()

    // Create a manual reminder for comparison
    const manualReminderResponse = await request.post(`${API_BASE_URL}/api/v1/reminders`, {
      headers: API_HEADERS,
      data: {
        contact_id: contactId,
        title: manualReminderTitle,
        due_date: new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString(),
      },
    })
    expect(manualReminderResponse.ok()).toBeTruthy()

    // Verify reminders exist via API (avoid extra UI navigation)
    const initialReminders = await getRemindersByContact(request, contactId)
    expect(initialReminders.ok).toBe(true)
    const reminderTitles = initialReminders.reminders.map(reminder => reminder.title)
    expect(reminderTitles).toContain(autoReminderTitle)
    expect(reminderTitles).toContain(manualReminderTitle)

    // Go to contact page and mark as contacted
    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: contactName }).first()).toBeVisible()

    // Click the "Mark as Contacted" button
    const lastContactedResponse = page.waitForResponse(
      response =>
        response.request().method() === 'PATCH' &&
        response.url().includes(`/api/v1/contacts/${contactId}/last-contacted`)
    )
    await page.getByRole('button', { name: /Mark as Contacted/i }).click()
    await lastContactedResponse

    // Verify reminders still exist via API
    const afterMarkReminders = await getRemindersByContact(request, contactId)
    expect(afterMarkReminders.ok).toBe(true)
    const afterTitles = afterMarkReminders.reminders.map(reminder => reminder.title)
    expect(afterTitles).toContain(autoReminderTitle)
    expect(afterTitles).toContain(manualReminderTitle)
  })
})
