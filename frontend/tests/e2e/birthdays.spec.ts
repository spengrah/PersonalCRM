import { test, expect } from '@playwright/test'
import type { Page } from '@playwright/test'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'

// Freeze the client-side accelerated-time frame to a fixed instant by mocking
// GET /system/time BEFORE the page loads. `acceleration_factor: 0` pins
// currentTime at base_time (a non-zero factor collapses to the wall clock), and
// the body is the full apiClient envelope (apiClient unwraps `data`, so a bare
// inner object would leave the frame undefined). Per-page + pre-navigation, so
// it is parallel-safe and does not touch the process-wide acceleration state.
// NOTE: page.route does NOT intercept the declared seed, so the birthdays these
// tests read are the declaration's own fixed dates, chosen against the mocked
// frame rather than the real one.
async function mockFrozenSystemTime(page: Page, isoInstant: string): Promise<void> {
  await page.route('**/api/v1/system/time', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        success: true,
        data: {
          current_time: isoInstant,
          base_time: isoInstant,
          is_accelerated: true,
          acceleration_factor: 0,
          environment: 'testing',
        },
      }),
    })
  })
}

test.describe('Birthdays - Placeholder Years @area:birthdays', () => {
  // The birthdays page classifies a birthday by comparing LOCAL calendar days
  // (startOfLocalDay over the accelerated frame), while the declared fixture's
  // "today" is derived from the run anchor in UTC. Pinning the browser to UTC
  // makes those the same day, including exactly at the UTC-midnight boundary
  // where an unpinned runner timezone would disagree by one.
  test.use({ timezoneId: 'UTC' })

  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // The month/day the placeholder-year fixture actually stored, mirroring the
  // Go-side clamp: a placeholder-year (1900, non-leap) birthday cannot represent
  // February 29, so BirthdayPlaceholderToday stores February 28 on that one
  // anchor. Read from the manifest's anchor in UTC, which is the same number the
  // lowering used.
  function placeholderMonthDay(seeded: SeedBehaviorResult): { month: number; day: number } {
    const anchor = new Date(seeded.anchor)
    const month = anchor.getUTCMonth() + 1
    const day = anchor.getUTCDate()
    if (month === 2 && day === 29) return { month: 2, day: 28 }
    return { month, day }
  }

  // Every locator below that resolves a fixture BY NAME matches EXACTLY. The
  // declared names are generator-drawn, and two of the eight contacts that draw
  // the same given+surname pair render "<name>" and "<name> N" — so a substring
  // match on the shorter one resolves both cards and fails strict mode.
  function isLeapDayAnchor(seeded: SeedBehaviorResult): boolean {
    const anchor = new Date(seeded.anchor)
    return anchor.getUTCMonth() === 1 && anchor.getUTCDate() === 29
  }

  test('shows placeholder-year birthdays without an age', async ({ page }) => {
    // spec: CON-045.placeholder-year-birthdays-no-age
    // spec: KNW-035.date-renders-month-day, KNW-035.no-age-computed-displayed
    // The list-row and detail-page assertions prove the year-less rendering
    // (month/day only, placeholder year never shown) and the birthdays-card
    // assertion proves the age suppression.
    //
    // This claim is DATE-INDEPENDENT and runs unconditionally, on every calendar
    // day, because it never depends on which section of the birthdays page the
    // card lands in. The separate, uncited test below covers the one claim that
    // genuinely cannot hold on a February 29 anchor.
    const seeded = await testApi.seedBehavior('CON-045')
    const contact = seeded.entities['real-today']
    const { month, day } = placeholderMonthDay(seeded)
    const birthdayDate = new Date(1900, month - 1, day, 12, 0, 0)
    const expectedListDate = `${birthdayDate.getMonth() + 1}/${birthdayDate.getDate()}`
    const expectedDetailDate = birthdayDate.toLocaleDateString('en-US', {
      month: 'short',
      day: 'numeric',
    })
    const expectedBirthdayPageDate = birthdayDate.toLocaleDateString('en-US', {
      month: 'long',
      day: 'numeric',
    })

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')
    await page.getByPlaceholder('Search contacts...').fill(contact.name)
    await page.getByPlaceholder('Search contacts...').press('Enter')
    const row = page.locator('tr', { has: page.getByText(contact.name, { exact: true }) })
    await expect(row).toBeVisible({ timeout: 15000 })
    await expect(row).toContainText(expectedListDate)
    await expect(row).not.toContainText('/00')
    await expect(row).not.toContainText('1900')

    await page.goto(`/contacts/${contact.id}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: contact.name })).toBeVisible({ timeout: 15000 })
    await expect(page.getByText(expectedDetailDate, { exact: true })).toBeVisible()
    await expect(page.getByText('1900')).not.toBeVisible()

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')
    // Deliberately NOT scoped to a section: the claim is "no age is shown", which
    // holds wherever the card lands — including the already-celebrated group on
    // the one anchor where the clamp moves it there.
    const card = page
      .getByTestId('birthday-card')
      .filter({ has: page.getByText(contact.name, { exact: true }) })
    await expect(card).toBeVisible({ timeout: 15000 })
    await expect(card).toContainText(expectedBirthdayPageDate)
    await expect(card).not.toContainText(/Turning|Turned/)
  })

  test('keeps an unmocked today placeholder-year birthday at the top', async ({ page }) => {
    // No spec citation, deliberately. This is bonus verification of the REAL
    // (unmocked, accelerated-clock-driven) today classification, beyond what
    // CON-045.contacts-grouped-into-today already proves against a mocked frame
    // below — and it is the only block here that may be skipped, precisely
    // because nothing cited depends on it. A skipped test that carried a
    // citation would keep the corpus green while proving nothing: spec-coverage
    // is a static scanner over citations and cannot see a skip.
    const seeded = await testApi.seedBehavior('CON-045')
    test.skip(
      isLeapDayAnchor(seeded),
      'a placeholder-year (1900, non-leap) birthday cannot represent February 29 at all, so the ' +
        'product has no valid state for "today is my year-unknown birthday" on this one real ' +
        'calendar day: the fixture clamps to February 28 for seed safety, which the app then ' +
        'correctly classifies as already-celebrated rather than today.'
    )

    const contact = seeded.entities['real-today']
    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')
    const todaySection = page.locator('section', {
      has: page.getByRole('heading', { name: /Today's Birthdays/ }),
    })
    await expect(todaySection).toBeVisible({ timeout: 15000 })
    const card = todaySection
      .getByTestId('birthday-card')
      .filter({ has: page.getByText(contact.name, { exact: true }) })
    await expect(card).toBeVisible()
    await expect(card).toContainText('Today!')
  })

  test('groups birthdays into today, upcoming, and already-celebrated', async ({ page }) => {
    // spec: CON-045.contacts-grouped-into-today
    // Freeze the frame mid-year so the three groups are deterministic and
    // parallel-safe (the same per-page frame-mock idiom used for CON-045.gift-planning-near-year-end).
    await mockFrozenSystemTime(page, '2026-06-15T12:00:00Z')
    const seeded = await testApi.seedBehavior('CON-045')
    const todayName = seeded.entities['mocked-today'].name
    const upcomingName = seeded.entities['mid'].name
    const celebratedName = seeded.entities['celebrated'].name

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')

    const todaySection = page.locator('section', {
      has: page.getByRole('heading', { name: /Today's Birthdays/ }),
    })
    const upcomingSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Upcoming Birthdays/ }),
    })
    const celebratedSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Already Celebrated This Year/ }),
    })

    await expect(todaySection).toBeVisible({ timeout: 15000 })
    await expect(upcomingSection).toBeVisible()
    await expect(celebratedSection).toBeVisible()

    // Each seeded contact lands in the correct group.
    await expect(todaySection.getByText(todayName, { exact: true })).toBeVisible()
    await expect(upcomingSection.getByText(upcomingName, { exact: true })).toBeVisible()
    await expect(celebratedSection.getByText(celebratedName, { exact: true })).toBeVisible()
  })

  test('sorts upcoming birthdays soonest-first and sinks celebrated to the end', async ({
    page,
  }) => {
    // spec: CON-045.upcoming-birthdays-sort-soonest
    await mockFrozenSystemTime(page, '2026-06-15T12:00:00Z')
    const seeded = await testApi.seedBehavior('CON-045')
    const soonName = seeded.entities['soon'].name // 3 days out
    const laterName = seeded.entities['later'].name // 10 days out
    const celebratedName = seeded.entities['celebrated'].name

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')

    const upcomingSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Upcoming Birthdays/ }),
    })
    const celebratedSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Already Celebrated This Year/ }),
    })
    await expect(upcomingSection).toBeVisible({ timeout: 15000 })
    await expect(celebratedSection).toBeVisible()

    // Within upcoming, the sooner birthday (3 days) precedes the later (10 days)
    // in DOM order — manifest card names, not viewport coordinates.
    await expect(upcomingSection.getByText(soonName, { exact: true })).toBeVisible()
    await expect(upcomingSection.getByText(laterName, { exact: true })).toBeVisible()
    // The card NAMES in DOM order, compared exactly: matching a card by substring
    // over its whole text would let the later card answer for the sooner one
    // whenever the two drawn names collide.
    const upcomingNames = await upcomingSection
      .getByTestId('birthday-card')
      .getByRole('heading', { level: 3 })
      .allTextContents()
    const soonIdx = upcomingNames.indexOf(soonName)
    const laterIdx = upcomingNames.indexOf(laterName)
    expect(soonIdx).toBeGreaterThanOrEqual(0)
    expect(laterIdx).toBeGreaterThan(soonIdx)

    // The seeded celebrated contact sits in the celebrated section, which renders
    // AFTER the upcoming section (section headings compared in DOM order).
    await expect(celebratedSection.getByText(celebratedName, { exact: true })).toBeVisible()
    const headings = await page.getByRole('heading', { level: 2 }).allTextContents()
    const upcomingHeadingIdx = headings.findIndex(h => /Upcoming Birthdays/.test(h))
    const celebratedHeadingIdx = headings.findIndex(h => /Already Celebrated This Year/.test(h))
    expect(upcomingHeadingIdx).toBeGreaterThanOrEqual(0)
    expect(celebratedHeadingIdx).toBeGreaterThan(upcomingHeadingIdx)
  })

  test('the birthdays page date header follows the server accelerated frame', async ({ page }) => {
    // spec: CON-045.page-follows-accelerated-time
    // Freeze the frame to a fixed, non-wall-clock date and assert the page
    // header renders THAT date — proving it follows the server frame rather than
    // the wall clock (a real-frame assertion would pass trivially when the
    // backend reports is_accelerated=false and the frame equals the wall clock).
    // Needs no seeded data at all.
    await mockFrozenSystemTime(page, '2026-09-03T12:00:00Z')
    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: 'Birthday Tracker' })).toBeVisible({
      timeout: 15000,
    })
    await expect(page.getByText(/September 3, 2026/)).toBeVisible()
  })

  test('shows the gift-planning section near year end', async ({ page }) => {
    // spec: CON-045.gift-planning-near-year-end
    // December frame → the page surfaces early-next-year (Jan-Mar) birthdays.
    await mockFrozenSystemTime(page, '2026-12-15T12:00:00Z')
    const seeded = await testApi.seedBehavior('CON-045')
    const febName = seeded.entities['gift-feb'].name

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')
    // Scope to the seeded contact INSIDE the gift-planning section — another
    // worker's Jan-Mar birthday could otherwise satisfy the bare heading.
    const giftSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Gift Planning/ }),
    })
    await expect(giftSection).toBeVisible({ timeout: 15000 })
    await expect(giftSection.getByText(febName, { exact: true })).toBeVisible()
  })

  test('a February 29 birthday is observed on February 29 in a leap year', async ({ page }) => {
    // spec: CON-045.leap-day-next-occurrence
    // The declared fixture stores February 29 against a REAL leap birth year:
    // the year-unknown placeholder is 1900, which is not a leap year, so
    // February 29 is not expressible as a month/day-only birthday at all.
    await mockFrozenSystemTime(page, '2028-02-29T12:00:00Z')
    const seeded = await testApi.seedBehavior('CON-045')
    const leapName = seeded.entities['leap-day'].name

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')

    const todaySection = page.locator('section', {
      has: page.getByRole('heading', { name: /Today's Birthdays/ }),
    })
    await expect(todaySection).toBeVisible({ timeout: 15000 })
    const card = todaySection
      .getByTestId('birthday-card')
      .filter({ has: page.getByText(leapName, { exact: true }) })
    await expect(card).toBeVisible()
    await expect(card).toContainText('February 29')
    await expect(card).toContainText('Today!')
  })

  test('a February 29 birthday is observed on March 1 in a common year', async ({ page }) => {
    // spec: CON-045.leap-day-next-occurrence
    // This is the case worth pinning ON THE PAGE. The page projects a stored
    // birthday onto the current year with new Date(year, month, day), which rolls
    // February 29 to March 1 in a common year — so from a February 27 frame the
    // next occurrence is TWO days out, not 367 and not "already celebrated".
    // Asserting it against the rendered DOM rather than against the Go mirror is
    // the point: the mirror is a restatement of this arithmetic, so agreeing with
    // it would prove only self-consistency.
    await mockFrozenSystemTime(page, '2027-02-27T12:00:00Z')
    const seeded = await testApi.seedBehavior('CON-045')
    const leapName = seeded.entities['leap-day'].name

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')

    const upcomingSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Upcoming Birthdays/ }),
    })
    await expect(upcomingSection).toBeVisible({ timeout: 15000 })
    const card = upcomingSection
      .getByTestId('birthday-card')
      .filter({ has: page.getByText(leapName, { exact: true }) })
    await expect(card).toBeVisible()
    await expect(card).toContainText('March 1')
    await expect(card).toContainText('2 days')

    // And it is NOT filed as already celebrated: the occurrence is ahead, not
    // behind, which is the half a naive Feb-28 clamp would get wrong.
    const celebratedSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Already Celebrated This Year/ }),
    })
    await expect(celebratedSection.getByText(leapName, { exact: true })).toHaveCount(0)
  })

  test('hides the gift-planning section away from year end', async ({ page }) => {
    // spec: CON-045.gift-planning-near-year-end
    // June frame → no gift-planning section, even with a Jan-Mar birthday.
    await mockFrozenSystemTime(page, '2026-06-15T12:00:00Z')
    const seeded = await testApi.seedBehavior('CON-045')
    const febName = seeded.entities['gift-feb'].name

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')
    // Wait for the sections to render (the Feb contact shows as celebrated),
    // then assert the gift-planning section is absent.
    await expect(page.getByText(febName, { exact: true })).toBeVisible({ timeout: 15000 })
    await expect(page.getByRole('heading', { name: /Gift Planning/ })).toHaveCount(0)
  })
})
