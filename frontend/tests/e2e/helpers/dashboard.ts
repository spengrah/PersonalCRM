import { expect, type Page, type Response } from '@playwright/test'

/**
 * Registers a content-predicate wait on GET /api/v1/contacts/overdue that
 * resolves once a successful response body contains every `presentIds` entry
 * and none of the `absentIds`. Reading the BODY (not just observing any
 * response) removes the ordering race against a pre-mutation fetch that still
 * carries a to-be-removed id — register BEFORE the triggering action/goto.
 */
export function waitForOverdueListSettled(
  page: Page,
  { absentIds = [], presentIds = [] }: { absentIds?: string[]; presentIds?: string[] }
): Promise<Response> {
  return page.waitForResponse(async response => {
    if (
      response.request().method() !== 'GET' ||
      !response.url().includes('/api/v1/contacts/overdue') ||
      !response.ok()
    ) {
      return false
    }
    const body = await response.json().catch(() => null)
    const entries: Array<{ id: string }> = body?.data ?? []
    return (
      absentIds.every(id => !entries.some(entry => entry.id === id)) &&
      presentIds.every(id => entries.some(entry => entry.id === id))
    )
  })
}

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
