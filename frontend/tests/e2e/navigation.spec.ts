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
      const pages = ['/dashboard', '/contacts', '/imports', '/settings', '/birthdays', '/reminders']

      for (const pagePath of pages) {
        await page.goto(pagePath)
        await page.waitForLoadState('domcontentloaded')

        const importsLink = page.getByRole('link', { name: /Imports/i })
        await expect(importsLink).toBeVisible()
      }
    })
  })

  test('navigation remains visible when scrolling', async ({ page }) => {
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
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // Verify the nav element has sticky positioning classes
    const nav = page.getByRole('navigation')
    await expect(nav).toHaveClass(/sticky/)
    await expect(nav).toHaveClass(/top-0/)
    await expect(nav).toHaveClass(/z-50/)
  })
})
