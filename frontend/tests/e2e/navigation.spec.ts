import { test, expect } from '@playwright/test'

test.describe('Navigation @area:navigation', () => {
  // Note: Sync indicator visual states (gray/green/red) are tested via unit tests for
  // getAggregateSyncStatus and getSyncIconClasses. Full E2E state testing would require
  // complex API mocking to simulate sync states, which isn't warranted for this nav indicator.
  // These E2E tests verify the infrastructure (icon presence, CSS animation) works correctly.
  test.describe('Sync Status Indicator', () => {
    test('imports nav item should have an icon element', async ({ page }) => {
      await page.goto('/dashboard')
      await page.waitForLoadState('domcontentloaded')

      // Find the Imports link in navigation
      const importsLink = page.getByRole('link', { name: /Imports/i })
      await expect(importsLink).toBeVisible()

      // Verify it contains an SVG icon (CloudDownload icon from lucide-react)
      const icon = importsLink.locator('svg')
      await expect(icon).toBeVisible()
      await expect(icon).toHaveClass(/w-4.*h-4|h-4.*w-4/)
    })

    test('sync-pulse animation is defined in CSS', async ({ page }) => {
      await page.goto('/dashboard')
      await page.waitForLoadState('domcontentloaded')

      // Verify the animate-sync-pulse class is available in the stylesheet
      // by checking that applying it would create an animation
      const hasAnimation = await page.evaluate(() => {
        // Create a test element with the class
        const testEl = document.createElement('div')
        testEl.className = 'animate-sync-pulse'
        document.body.appendChild(testEl)

        // Get computed animation
        const computed = window.getComputedStyle(testEl)
        const animation = computed.animation || computed.getPropertyValue('animation')
        document.body.removeChild(testEl)

        // Check if animation is defined (not 'none' or empty)
        return animation && animation !== 'none' && animation.includes('sync-pulse')
      })

      expect(hasAnimation).toBe(true)
    })

    test('imports link should be accessible from all main pages', async ({ page }) => {
      const pages = ['/dashboard', '/contacts', '/imports', '/settings', '/birthdays']

      for (const pagePath of pages) {
        await page.goto(pagePath)
        await page.waitForLoadState('domcontentloaded')

        const importsLink = page.getByRole('link', { name: /Imports/i })
        await expect(importsLink).toBeVisible()
      }
    })
  })

  test('persistent nav links all five sections and marks the current one active', async ({
    page,
  }) => {
    // spec: DSH-002[0], DSH-002[1]
    // On EVERY primary surface, all five section links must be present with
    // their correct hrefs (DSH-002[0]), and the link matching the current
    // section must carry the active mark — border-blue-500, an aria-invisible
    // visual-state fact asserted as a class (mirroring the sticky-classes test
    // below) — while a non-current link does not (DSH-002[1]). Asserting the
    // active token per route proves the mark follows the pathname rather than
    // a hard-coded default.
    const SECTIONS = [
      { name: 'Dashboard', href: '/dashboard' },
      { name: 'Contacts', href: '/contacts' },
      { name: 'Birthdays', href: '/birthdays' },
      { name: 'Imports', href: '/imports' },
      { name: 'Settings', href: '/settings' },
    ]

    for (const current of SECTIONS) {
      await page.goto(current.href)
      await page.waitForLoadState('domcontentloaded')

      const nav = page.getByRole('navigation')
      for (const section of SECTIONS) {
        const link = nav.getByRole('link', { name: section.name, exact: true })
        await expect(link).toBeVisible()
        await expect(link).toHaveAttribute('href', section.href)
      }

      const activeLink = nav.getByRole('link', { name: current.name, exact: true })
      await expect(activeLink).toHaveClass(/border-blue-500/)
      const other = current.href === SECTIONS[0].href ? SECTIONS[1] : SECTIONS[0]
      const inactiveLink = nav.getByRole('link', { name: other.name, exact: true })
      await expect(inactiveLink).not.toHaveClass(/border-blue-500/)
    }
  })

  test('navigation remains visible when scrolling', async ({ page }) => {
    // spec: DSH-002[2]
    // Navigate to contacts page (has enough content to scroll)
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Verify nav is initially visible using accessibility-focused selector
    const nav = page.getByRole('navigation')
    await expect(nav).toBeVisible()

    // Add content to make the page scrollable if needed
    await page.evaluate(() => {
      document.body.style.minHeight = '200vh'
    })

    // Scroll down significantly
    await page.evaluate(() => window.scrollTo(0, 500))

    // Wait for scroll position to actually change (deterministic wait)
    await page.waitForFunction(() => window.scrollY >= 400)

    // Verify nav is still visible after scrolling
    await expect(nav).toBeVisible()

    // Verify nav is at top of viewport (sticky behavior)
    // Use tolerance to account for browser differences
    const navBox = await nav.boundingBox()
    expect(navBox).not.toBeNull()
    expect(navBox?.y).toBeLessThanOrEqual(5)
  })

  test('navigation has correct sticky classes', async ({ page }) => {
    // spec: DSH-002[2]
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // Verify the nav element has sticky positioning classes
    const nav = page.getByRole('navigation')
    await expect(nav).toHaveClass(/sticky/)
    await expect(nav).toHaveClass(/top-0/)
    await expect(nav).toHaveClass(/z-50/)
  })
})
