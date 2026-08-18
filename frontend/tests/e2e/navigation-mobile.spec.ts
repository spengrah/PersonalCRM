import { test, expect } from '@playwright/test'

// Phone-width navigation: below Tailwind's `sm` breakpoint the inline link
// list is hidden (`hidden sm:flex`), so every section must be reachable
// through the hamburger disclosure panel instead.
test.describe('Mobile navigation @area:navigation', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true })

  test('hamburger menu opens, navigates to every section, and closes on selection', async ({
    page,
  }) => {
    // spec: DSH-002.narrow-viewport-menu
    const SECTIONS = [
      { name: 'Dashboard', href: '/dashboard' },
      { name: 'Contacts', href: '/contacts' },
      { name: 'Birthdays', href: '/birthdays' },
      { name: 'Imports', href: '/imports' },
      { name: 'Settings', href: '/settings' },
    ]

    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    const menuButton = page.getByRole('button', { name: 'Open main menu' })
    await expect(menuButton).toBeVisible()
    await expect(menuButton).toHaveAttribute('aria-expanded', 'false')

    const panel = page.locator('#mobile-menu')
    for (const section of SECTIONS) {
      await menuButton.click()
      await expect(menuButton).toHaveAttribute('aria-expanded', 'true')
      await expect(panel).toBeVisible()

      await panel.getByRole('link', { name: section.name, exact: true }).click()
      await expect(page).toHaveURL(new RegExp(`${section.href}([?/]|$)`))
      // Selecting a link closes the panel (it unmounts entirely).
      await expect(panel).toHaveCount(0)
      await expect(menuButton).toHaveAttribute('aria-expanded', 'false')
    }
  })
})
