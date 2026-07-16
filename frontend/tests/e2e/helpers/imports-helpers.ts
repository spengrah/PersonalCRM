import { Page, Locator, expect } from '@playwright/test'

/** Candidate card scoped by its heading (never by presentation classes). */
export function candidateCardByName(page: Page, displayName: string): Locator {
  return page
    .locator('div.border', { has: page.getByRole('heading', { name: displayName }) })
    .first()
}

/** The candidate-resolution modal body. */
export function resolverDialog(page: Page): Locator {
  return page.getByRole('dialog', { name: 'Resolve import candidate' })
}

/**
 * Select a contact in the resolution modal's ContactSelector when nothing is
 * pre-selected. When CLOSED with no selection, the selector renders the
 * placeholder as a SPAN (not an input) — so detect that text, click to open,
 * then type into the opened input and pick the option.
 */
export async function selectContactIfNeeded(
  page: Page,
  dialog: Locator,
  searchTerm: string,
  optionName: string
): Promise<void> {
  const closedPlaceholder = dialog.getByText('Search for a contact...')
  if (await closedPlaceholder.isVisible().catch(() => false)) {
    await closedPlaceholder.click()
    await dialog.getByPlaceholder('Search for a contact...').fill(searchTerm)
    await page.getByText(optionName, { exact: true }).last().click()
  }
}

/**
 * Helper to navigate the modal to show a specific candidate by name.
 * This handles the race condition where parallel tests can cause the modal
 * to open showing a different candidate than the one clicked.
 *
 * Strategy: Go to start first (all prev), then scan forward (all next).
 * This ensures we check every candidate exactly once.
 */
export async function navigateModalToCandidate(
  page: Page,
  displayName: string,
  maxNavigations = 30
): Promise<void> {
  const modal = page.getByRole('dialog', { name: 'Resolve import candidate' })
  const prevButton = page.getByRole('button', { name: 'Previous candidate' })
  const nextButton = page.getByRole('button', { name: 'Next candidate' })

  // Helper to check if candidate is visible
  const isTargetVisible = async () => {
    const modalHeading = modal.getByRole('heading', { level: 3, name: displayName })
    return modalHeading.isVisible({ timeout: 300 }).catch(() => false)
  }

  // Check if already showing correct candidate
  if (await isTargetVisible()) return

  // Helper to wait for heading to change after navigation
  const waitForHeadingChange = async (previousHeading: string | null) => {
    if (previousHeading) {
      // Wait for the heading text to change (indicates navigation completed)
      await expect(modal.getByRole('heading', { level: 3 }))
        .not.toHaveText(previousHeading, { timeout: 2000 })
        .catch(() => {}) // Ignore timeout if heading doesn't change
    }
  }

  // Phase 1: Go to start (click prev until disabled)
  for (let i = 0; i < maxNavigations; i++) {
    const prevVisible = await prevButton.isVisible({ timeout: 300 }).catch(() => false)
    if (!prevVisible) break
    const prevDisabled = await prevButton.isDisabled()
    if (prevDisabled) break
    const headingBefore = await modal.getByRole('heading', { level: 3 }).textContent()
    await prevButton.click()
    await waitForHeadingChange(headingBefore)
    if (await isTargetVisible()) return
  }

  // Phase 2: Scan forward (click next until found or disabled)
  for (let i = 0; i < maxNavigations; i++) {
    const nextVisible = await nextButton.isVisible({ timeout: 300 }).catch(() => false)
    if (!nextVisible) break
    const nextDisabled = await nextButton.isDisabled()
    if (nextDisabled) break
    const headingBefore = await modal.getByRole('heading', { level: 3 }).textContent()
    await nextButton.click()
    await waitForHeadingChange(headingBefore)
    if (await isTargetVisible()) return
  }

  // Final check - verify we found the candidate
  const modalHeading = modal.getByRole('heading', { level: 3, name: displayName })
  await expect(modalHeading).toBeVisible({ timeout: 2000 })
}

/**
 * Helper to find a candidate by name, handling pagination if needed.
 * Returns the candidate card locator once found.
 */
export async function findCandidateByName(
  page: Page,
  displayName: string,
  maxPages = 5
): Promise<void> {
  for (let i = 0; i < maxPages; i++) {
    // Check if our contact is visible on current page (use heading to avoid matching Link button)
    const contactHeading = page.getByRole('heading', { name: displayName })
    if (await contactHeading.isVisible({ timeout: 2000 }).catch(() => false)) {
      return
    }

    // Try to go to next page (use exact match to avoid matching "Next candidate" in modal)
    const nextButton = page.getByRole('button', { name: 'Next', exact: true })
    if (await nextButton.isVisible({ timeout: 1000 }).catch(() => false)) {
      const isDisabled = await nextButton.isDisabled()
      if (!isDisabled) {
        await nextButton.click()
        await page.waitForLoadState('networkidle')
        continue
      }
    }

    // No more pages, contact not found
    break
  }

  // Final check - if still not visible, the expect will fail with a good error
  await expect(page.getByRole('heading', { name: displayName })).toBeVisible({ timeout: 5000 })
}
