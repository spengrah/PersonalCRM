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
 * Assert the resolver modal is showing the expected candidate. The modal is
 * keyed by the clicked card's candidate id, so it deterministically opens on
 * that candidate even when a list refetch reorders the queue — this expect
 * fails loudly if that regresses, instead of letting a test act on the
 * wrong candidate.
 */
export async function expectModalCandidate(page: Page, displayName: string): Promise<void> {
  await expect(
    resolverDialog(page).getByRole('heading', { level: 3, name: displayName })
  ).toBeVisible({ timeout: 5000 })
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
