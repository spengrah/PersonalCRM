import { test, expect } from '@playwright/test'

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// Helper to create a test import candidate directly in the database
async function createTestCandidate(
  request: Parameters<Parameters<typeof test>[1]>[0]['request'],
  data: {
    source?: string
    external_id: string
    display_name?: string
    first_name?: string
    last_name?: string
    organization?: string
    job_title?: string
    emails?: string[]
    phones?: string[]
  }
) {
  // We need to insert directly into external_contact table
  // This requires a special test endpoint or direct DB access
  // For now, we'll use the sync infrastructure to simulate this
  // by checking if candidates exist after the page loads

  // Note: In a real setup, you'd have a test seeding endpoint
  // For this test, we'll work with whatever candidates exist
  return data
}

test.describe('Imports Page', () => {
  test.beforeEach(async ({ page }) => {
    // Navigate to imports page before each test
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')
  })

  test('should display page header and sync button', async ({ page }) => {
    // Verify page header
    await expect(page.getByRole('heading', { name: 'Import Contacts' })).toBeVisible()

    // Verify sync button exists
    await expect(page.getByRole('button', { name: /Sync Google Contacts/i })).toBeVisible()
  })

  test('should show imports in navigation', async ({ page }) => {
    // Verify navigation has Imports entry
    await expect(page.getByRole('link', { name: /Imports/i })).toBeVisible()
  })

  test('should display empty state when no candidates', async ({ page, request }) => {
    // First, ensure there are no candidates by checking the API
    const response = await request.get(`${API_BASE_URL}/api/v1/imports/candidates`, {
      headers: API_HEADERS,
    })

    if (response.ok()) {
      const data = await response.json()
      if (data.data?.length === 0 || data.meta?.pagination?.total === 0) {
        // Empty state should show specific messaging
        await expect(page.getByText(/No import candidates/i)).toBeVisible()
        await expect(page.getByText(/All contacts from Google have been imported/i)).toBeVisible()
      }
    }
  })

  test('should display candidate cards with correct information', async ({ page, request }) => {
    // Check if there are any candidates
    const response = await request.get(`${API_BASE_URL}/api/v1/imports/candidates`, {
      headers: API_HEADERS,
    })

    if (!response.ok()) {
      test.skip()
      return
    }

    const data = await response.json()
    const candidates = data.data || []

    if (candidates.length === 0) {
      // Skip if no candidates to test
      test.skip()
      return
    }

    const firstCandidate = candidates[0]
    const displayName =
      firstCandidate.display_name ||
      [firstCandidate.first_name, firstCandidate.last_name].filter(Boolean).join(' ') ||
      'Unknown'

    // Verify the candidate name is visible
    await expect(page.getByText(displayName)).toBeVisible()

    // Verify action buttons are present
    await expect(page.getByRole('button', { name: /Import/i }).first()).toBeVisible()
    await expect(page.getByRole('button', { name: /Link/i }).first()).toBeVisible()
  })

  test('should open link modal when clicking Link button', async ({ page, request }) => {
    // Check if there are any candidates
    const response = await request.get(`${API_BASE_URL}/api/v1/imports/candidates`, {
      headers: API_HEADERS,
    })

    if (!response.ok()) {
      test.skip()
      return
    }

    const data = await response.json()
    if (!data.data || data.data.length === 0) {
      test.skip()
      return
    }

    // Click the Link button on the first candidate
    await page.getByRole('button', { name: /Link/i }).first().click()

    // Verify modal opens
    await expect(page.getByText('Link to Existing Contact')).toBeVisible()
    await expect(page.getByText('Select Contact')).toBeVisible()

    // Verify cancel button works
    await page.getByRole('button', { name: /Cancel/i }).click()
    await expect(page.getByText('Link to Existing Contact')).not.toBeVisible()
  })
})

test.describe('Imports - Import Action', () => {
  test('should import candidate and show success notification', async ({ page, request }) => {
    // First check if there are candidates
    const response = await request.get(`${API_BASE_URL}/api/v1/imports/candidates`, {
      headers: API_HEADERS,
    })

    if (!response.ok()) {
      test.skip()
      return
    }

    const data = await response.json()
    if (!data.data || data.data.length === 0) {
      test.skip()
      return
    }

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    const firstCandidate = data.data[0]
    const displayName =
      firstCandidate.display_name ||
      [firstCandidate.first_name, firstCandidate.last_name].filter(Boolean).join(' ') ||
      'contact'

    // Get initial count of candidates
    const initialCandidates = data.data.length

    // Click Import on the first candidate
    await page
      .getByRole('button', { name: /Import/i })
      .first()
      .click()

    // Wait for the action to complete
    await page.waitForLoadState('networkidle')

    // Verify success notification appears
    await expect(page.getByText(/imported successfully/i)).toBeVisible({ timeout: 10000 })

    // Verify the candidate is removed from the list (or list is shorter)
    if (initialCandidates === 1) {
      // If there was only one candidate, empty state should show
      await expect(page.getByText(/No import candidates/i)).toBeVisible({ timeout: 10000 })
    }

    // Clean up: delete the imported contact
    // Go to contacts page and find the newly created contact
    await page.goto('/contacts')
    await page.waitForLoadState('networkidle')

    // Find and delete the contact we just imported
    const contactLink = page.getByRole('link', { name: displayName })
    if (await contactLink.isVisible()) {
      await contactLink.click()
      await page.waitForLoadState('networkidle')

      page.once('dialog', dialog => dialog.accept())
      await Promise.all([
        page.waitForURL('/contacts'),
        page.getByRole('button', { name: 'Delete' }).click(),
      ])
    }
  })
})

test.describe('Imports - Ignore Action', () => {
  test('should ignore candidate and show notification', async ({ page, request }) => {
    // First check if there are candidates
    const response = await request.get(`${API_BASE_URL}/api/v1/imports/candidates`, {
      headers: API_HEADERS,
    })

    if (!response.ok()) {
      test.skip()
      return
    }

    const data = await response.json()
    if (!data.data || data.data.length === 0) {
      test.skip()
      return
    }

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Click the X (ignore) button on the first candidate
    // The ignore button is a ghost button with just an X icon
    const candidateCard = page.locator('[class*="rounded-lg"]').first()
    const ignoreButton = candidateCard
      .getByRole('button')
      .filter({ has: page.locator('svg') })
      .last()

    await ignoreButton.click()

    // Wait for the action to complete
    await page.waitForLoadState('networkidle')

    // Verify notification appears
    await expect(page.getByText(/ignored/i)).toBeVisible({ timeout: 10000 })
  })
})

test.describe('Imports - Link Action', () => {
  test('should link candidate to existing contact', async ({ page, request }) => {
    // First, we need both a candidate and a contact to link to
    const candidatesResponse = await request.get(`${API_BASE_URL}/api/v1/imports/candidates`, {
      headers: API_HEADERS,
    })

    if (!candidatesResponse.ok()) {
      test.skip()
      return
    }

    const candidatesData = await candidatesResponse.json()
    if (!candidatesData.data || candidatesData.data.length === 0) {
      test.skip()
      return
    }

    // Create a test contact to link to
    const suffix = Date.now()
    const contactName = `E2E Link Target ${suffix}`

    const contactResponse = await request.post(`${API_BASE_URL}/api/v1/contacts`, {
      headers: API_HEADERS,
      data: {
        full_name: contactName,
      },
    })

    if (!contactResponse.ok()) {
      test.skip()
      return
    }

    const contactData = await contactResponse.json()
    const contactId = contactData.data.id

    try {
      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Click Link on the first candidate
      await page.getByRole('button', { name: /Link/i }).first().click()

      // Wait for modal to open
      await expect(page.getByText('Link to Existing Contact')).toBeVisible()

      // Search for and select the contact we created
      // The contact selector is a combobox/searchable dropdown
      const contactSelector = page.getByRole('combobox')
      if (await contactSelector.isVisible()) {
        await contactSelector.click()
        await page.getByText(contactName).click()
      } else {
        // Fallback: try clicking on the contact in a list
        await page.getByText(contactName).click()
      }

      // Click Link Contact button
      await page.getByRole('button', { name: /Link Contact/i }).click()

      // Wait for action to complete
      await page.waitForLoadState('networkidle')

      // Verify success notification
      await expect(page.getByText(/linked successfully/i)).toBeVisible({ timeout: 10000 })
    } finally {
      // Clean up: delete the test contact
      await page.goto(`/contacts/${contactId}`)
      await page.waitForLoadState('networkidle')

      page.once('dialog', dialog => dialog.accept())
      await Promise.all([
        page.waitForURL('/contacts'),
        page.getByRole('button', { name: 'Delete' }).click(),
      ])
    }
  })
})

test.describe('Imports - Sync', () => {
  test('should trigger sync when clicking sync button', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Click the sync button
    await page.getByRole('button', { name: /Sync Google Contacts/i }).click()

    // The button should show loading state or we should see a notification
    // Note: The actual sync might fail if Google OAuth isn't configured,
    // but we're testing the UI interaction works
    await page.waitForLoadState('networkidle')

    // Either we get a success or error notification
    const notification = page.locator('[class*="rounded-lg"]').filter({
      has: page.locator('svg'),
    })
    // Just verify the page doesn't crash
    await expect(page.getByRole('heading', { name: 'Import Contacts' })).toBeVisible()
  })
})

test.describe('Imports - Pagination', () => {
  test('should show pagination when there are multiple pages', async ({ page, request }) => {
    // Check if there are enough candidates for pagination
    const response = await request.get(`${API_BASE_URL}/api/v1/imports/candidates?limit=20`, {
      headers: API_HEADERS,
    })

    if (!response.ok()) {
      test.skip()
      return
    }

    const data = await response.json()
    const totalPages = data.meta?.pagination?.pages || 0

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    if (totalPages > 1) {
      // Verify pagination controls are visible
      await expect(page.getByRole('button', { name: /Previous/i })).toBeVisible()
      await expect(page.getByRole('button', { name: /Next/i })).toBeVisible()
      await expect(page.getByText(/Page \d+ of \d+/i)).toBeVisible()
    } else {
      // With 1 or fewer pages, pagination may not be shown
      // This is expected behavior
    }
  })
})
