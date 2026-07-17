import { Page, Locator, expect } from '@playwright/test'

/**
 * Candidate card scoped by its heading (never by presentation classes). The
 * heading match is exact: a substring match could also match a foreign
 * worker's card whose heading merely contains this name, which `.first()`
 * would then silently prefer.
 */
export function candidateCardByName(page: Page, displayName: string): Locator {
  return page
    .locator('div.border', { has: page.getByRole('heading', { name: displayName, exact: true }) })
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
 *
 * The candidates endpoint re-sorts the ENTIRE live global pool (confidence,
 * then alphabetical) on every single page fetch, since scores are computed
 * in-memory. Under real concurrent seeding from other workers, the target
 * can shift pages between one page's fetch and the next, so a single
 * forward-only pass can walk right past it. Wrapping the whole scan in
 * `toPass` retries the full page-1-through-maxPages walk (not just one
 * page) until it succeeds or the overall budget runs out, so a resort
 * mid-scan self-heals on the next attempt instead of failing outright.
 */
export async function findCandidateByName(
  page: Page,
  displayName: string,
  maxPages = 5
): Promise<void> {
  const contactHeading = page.getByRole('heading', { name: displayName, exact: true })
  // isVisible() never waits (its timeout option is deprecated and ignored),
  // so probes must go through waitFor to actually give the DOM time to
  // render before we conclude "not here" and paginate past the target.
  const seen = (locator: Locator, timeout: number) =>
    locator
      .waitFor({ state: 'visible', timeout })
      .then(() => true)
      .catch(() => false)

  let attempt = 0
  await expect(async () => {
    attempt++
    if (attempt > 1) {
      // A previous attempt's clicks may have left the UI mid-pagination.
      const firstPageButton = page.getByRole('button', { name: '1', exact: true }).first()
      if (await seen(firstPageButton, 500)) {
        await firstPageButton.click()
        await page.waitForLoadState('networkidle')
      } else {
        // Pagination is absent: either the pool now fits one page (fine) or
        // a mid-walk shrink stranded the client on an empty page-N with no
        // rewind control (the page keeps its page param; the Pagination
        // component unmounts at pages <= 1). Reload to reset the client's
        // page state to 1 before re-checking.
        await page.reload()
        await page.waitForLoadState('domcontentloaded')
      }
    }

    for (let i = 0; i < maxPages; i++) {
      if (await seen(contactHeading, 1000)) {
        return
      }

      // Try to go to next page (use exact match to avoid matching "Next candidate" in modal)
      const nextButton = page.getByRole('button', { name: 'Next', exact: true })
      if (await seen(nextButton, 500)) {
        const isDisabled = await nextButton.isDisabled()
        if (!isDisabled) {
          await nextButton.click()
          await page.waitForLoadState('networkidle')
          continue
        }
      }

      // No more pages, contact not found on this attempt.
      break
    }

    await expect(contactHeading).toBeVisible({ timeout: 1000 })
  }).toPass({ timeout: 20000 })
}
