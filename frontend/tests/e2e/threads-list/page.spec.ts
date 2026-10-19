import { test, expect } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'

test.describe('Interactions page @area:interactions', () => {
  test('holds the heading, the Review switch and two tabs, the tab part of its address', async ({
    page,
  }) => {
    await page.goto('/interactions')
    const heading = page.getByRole('heading', { name: 'Interactions', level: 2 })
    const reviewSwitch = page.getByRole('switch', { name: 'Review' })
    const tabs = page.getByRole('tablist').getByRole('tab')

    // spec: IXN-074.heading-switch-and-tabs
    await expect(heading).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await expect(tabs).toHaveText(['Interactions', 'Threads'])
    const [headingBox, switchBox] = await Promise.all([
      heading.boundingBox(),
      reviewSwitch.boundingBox(),
    ])
    expect(switchBox!.x, 'the switch sits at the right of the heading').toBeGreaterThan(
      headingBox!.x + headingBox!.width
    )

    // spec: IXN-074.no-tab-opens-interactions
    await expect(page.getByRole('tab', { name: 'Interactions' })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    await expect(page.getByRole('tab', { name: 'Threads' })).toHaveAttribute(
      'aria-selected',
      'false'
    )

    // spec: IXN-074.tab-in-address
    await page.getByRole('tab', { name: 'Threads' }).click()
    await expect(page).toHaveURL(/\/interactions\?tab=threads$/)
    await expect(page.getByRole('tab', { name: 'Threads' })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    await page.reload()
    await expect(page.getByRole('tab', { name: 'Threads' })).toHaveAttribute(
      'aria-selected',
      'true',
      { timeout: SLOW_TIMEOUT_MS }
    )
    await expect(page.getByRole('tabpanel', { name: 'Threads' })).toBeVisible()
    await page.getByRole('tab', { name: 'Interactions' }).click()
    await expect(page).toHaveURL(/\/interactions$/)
    await expect(page.getByRole('tabpanel', { name: 'All interactions' })).toBeVisible()
  })
})
