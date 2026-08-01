import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

test.describe('Contact Direction Signals @area:contacts', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('shows direction signal timestamps after mutual interaction', async ({ page }) => {
    // spec: CAD-029.last-outreach-time-shown, CAD-029.last-response-time-shown
    // The declared fixture's mutual contact carries a past calendar meeting; the
    // calendar sync records an attended event as MUTUAL, and mutual is the one
    // direction that bumps last_outreach_at AND last_response_at together.
    const seeded = await testApi.seedBehavior('CAD-029')
    const contactId = seeded.entities['mutual'].id
    const fullName = seeded.entities['mutual'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    // Wait for contact data to load
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({
      timeout: 15000,
    })

    // Direction signals section should appear with outreach and response
    // TIMES, not just the labels — the value after the colon must be
    // non-empty (formatRelativeTime renders e.g. "3 days ago"; a blank
    // value would render "Last outreach: " and fail the \S+ match).
    await expect(page.getByText(/Last outreach: \S+/)).toBeVisible({ timeout: 5000 })
    await expect(page.getByText(/Last response: \S+/)).toBeVisible()
  })

  test('shows outreach but not response after outbound-only interaction', async ({ page }) => {
    // spec: CAD-029.last-outreach-time-shown, CAD-029.last-response-time-shown
    // The outbound-only contact carries a replayed OUTBOUND email and nothing
    // else: an outbound bumps last_outreach_at and touches neither
    // last_contacted nor last_response_at.
    const seeded = await testApi.seedBehavior('CAD-029')
    const contactId = seeded.entities['outbound-only'].id
    const fullName = seeded.entities['outbound-only'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({
      timeout: 15000,
    })

    // Should show the outreach TIME (non-empty value after the label)
    await expect(page.getByText(/Last outreach: \S+/)).toBeVisible({ timeout: 5000 })

    // Shown-IFF-exists: with no inbound/mutual interaction there is no
    // response timestamp, so the response line must be ABSENT.
    await expect(page.getByText('Last response:')).not.toBeVisible()
  })

  test('shows awaiting-reply indicator while a follow-up pends', async ({ page }) => {
    // spec: CAD-029.awaiting-reply-indicator-shown
    // has_pending_followup is computed LIVE from the contact's follow-up-loop
    // task row (its lifecycle + state), so the state IS reachable by seeding:
    // the declared fixture's awaiting contact carries a real outbound and a real
    // live follow-up hung on it, in that order, because a follow-up loop is
    // opened BY an outbound.
    const seeded = await testApi.seedBehavior('CAD-029')
    const contactId = seeded.entities['awaiting'].id
    const fullName = seeded.entities['awaiting'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({
      timeout: 15000,
    })

    // The awaiting-reply indicator renders while the follow-up pends, targeted
    // by its title attribute because that attribute is its only text content.
    await expect(page.getByTitle('Awaiting reply')).toBeVisible()
    await expect(page.getByText(/Last outreach: \S+/)).toBeVisible()
  })

  test('shows explicit no-recent-activity state with no direction signals', async ({ page }) => {
    // spec: CAD-029.explicit-no-recent-activity
    // The quiet contact has a cadence, no interactions and no follow-up, which
    // guarantees the no-recent-activity branch (no vacuous fallback).
    const seeded = await testApi.seedBehavior('CAD-029')
    const contactId = seeded.entities['quiet'].id
    const fullName = seeded.entities['quiet'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({
      timeout: 15000,
    })

    await expect(page.getByText('No recent activity')).toBeVisible()
    await expect(page.getByText('Last outreach:')).not.toBeVisible()
    await expect(page.getByText('Last response:')).not.toBeVisible()
    await expect(page.getByTitle('Awaiting reply')).not.toBeVisible()
  })

  // The interaction/contact wire-shape checks (direction field on POST/list,
  // direction timestamp fields on GET) that used to live here involved no page
  // and are owned by the Go API suite: TestInteractionAPI_DirectionInResponse,
  // TestContactAPI_DirectionTimestamps, and TestContactAPI_HasPendingFollowup
  // in backend/tests/api/direction_api_test.go.
})
