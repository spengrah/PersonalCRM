import { test as base, expect } from '@playwright/test'

/**
 * Custom test fixture that sets window.__PLAYWRIGHT__ before each page load.
 * This tells React Query to use staleTime: 0, ensuring tests always get fresh data.
 *
 * Usage: Import { test, expect } from './fixtures' instead of '@playwright/test'
 */
export const test = base.extend({
  page: async ({ page }, use) => {
    // Inject the flag before any page loads
    await page.addInitScript(() => {
      ;(window as Window & { __PLAYWRIGHT__?: boolean }).__PLAYWRIGHT__ = true
    })
    await use(page)
  },
})

export { expect }
export type { Locator } from '@playwright/test'
