import { test, expect } from '@playwright/test'

test.describe('Navigation', () => {
  test('navigation remains visible when scrolling', async ({ page }) => {
    // Navigate to contacts page (has enough content to scroll)
    await page.goto('/contacts')
    await page.waitForLoadState('networkidle')

    // Verify nav is initially visible
    const nav = page.locator('nav')
    await expect(nav).toBeVisible()

    // Add content to make the page scrollable if needed
    await page.evaluate(() => {
      document.body.style.minHeight = '200vh'
    })

    // Scroll down significantly
    await page.evaluate(() => window.scrollTo(0, 500))

    // Wait for scroll to complete
    await page.waitForTimeout(100)

    // Verify nav is still visible after scrolling
    await expect(nav).toBeVisible()

    // Verify nav is at top of viewport (sticky behavior)
    const navBox = await nav.boundingBox()
    expect(navBox).not.toBeNull()
    expect(navBox?.y).toBe(0)
  })

  test('navigation has correct sticky classes', async ({ page }) => {
    await page.goto('/dashboard')
    await page.waitForLoadState('networkidle')

    // Verify the nav element has sticky positioning classes
    const nav = page.locator('nav')
    await expect(nav).toHaveClass(/sticky/)
    await expect(nav).toHaveClass(/top-0/)
    await expect(nav).toHaveClass(/z-50/)
  })
})
