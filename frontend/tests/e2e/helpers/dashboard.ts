import { expect, type Page } from '@playwright/test'

/**
 * Asserts the dashboard header's add-contact CTA is present and correctly
 * targeted. The header renders OUTSIDE the overdue widget's
 * loading/error/caught-up/populated branches (dashboard/page.tsx), so the CTA
 * must be available in ANY dashboard state. Callers first establish the state
 * under test, then invoke this to prove the affordance survives it.
 */
export async function expectAddContactHeader(page: Page): Promise<void> {
  const addContact = page.getByRole('link', { name: 'Add Contact', exact: true })
  await expect(addContact).toBeVisible()
  await expect(addContact).toHaveAttribute('href', '/contacts/new')
}
