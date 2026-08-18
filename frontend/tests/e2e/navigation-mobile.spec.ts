import { test, expect } from '@playwright/test'
import { createTestAPI, declaredWorldSearch } from './helpers/test-api'

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

    const menuButton = page.getByRole('button', { name: 'Main menu' })
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

  // Groundwork regression guard for the phone-width layout fixes (uncited):
  // the contacts table scrolls in its own container instead of clipping, the
  // detail header's actions stay reachable, and neither page overflows the
  // viewport sideways.
  test('phone-width contacts list and detail keep actions reachable without page overflow @area:contacts', async ({
    page,
    request,
  }, testInfo) => {
    const testApi = createTestAPI(request, testInfo)

    const expectNoPageOverflow = async () => {
      const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth
      )
      expect(overflow, 'page must not scroll horizontally').toBeLessThanOrEqual(0)
    }

    try {
      const seeded = await testApi.seedBehavior('CON-042')
      const contactId = seeded.entities['target'].id
      const fullName = seeded.entities['target'].name
      // Contacts list: the wide table lives in a horizontal scroll container,
      // so the trailing row-actions column is reachable (clicking auto-scrolls
      // the container) while the page itself stays viewport-wide.
      await page.goto(`/contacts?search=${encodeURIComponent(declaredWorldSearch(seeded))}`)
      await expect(page.getByText(fullName)).toBeVisible({ timeout: 15000 })
      await expectNoPageOverflow()

      await page.getByRole('button', { name: 'Contact actions' }).first().click()
      await expect(page.getByRole('menu')).toBeVisible()
      await page.keyboard.press('Escape')

      // Contact detail: all four header actions sit inside the viewport (the
      // row wraps instead of running off the right edge).
      await page.goto(`/contacts/${contactId}`)
      await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
      for (const name of ['Log Interaction', 'Edit', 'Merge', 'Delete']) {
        await expect(page.getByRole('button', { name, exact: true }).first()).toBeInViewport()
      }
      await expectNoPageOverflow()
    } finally {
      await testApi.cleanup()
    }
  })
})
