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
  // EXACT, because this assertion's whole job is to prove the modal opened on the
  // RIGHT candidate. Generated candidate names are "…Import Candidate <seq>", so
  // a substring match lets an open on "Import Candidate 10" silently satisfy an
  // assertion for "Import Candidate 1" — a false PASS, which is the one failure
  // mode a guard like this must not have.
  await expect(
    resolverDialog(page).getByRole('heading', { level: 3, name: displayName, exact: true })
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
      // The probe doubles as the post-click settle: after a pagination
      // click the fetch+render completes well within this window. Never
      // wait on networkidle here — under parallel-worker load the network
      // rarely goes silent and the wait can eat the whole toPass budget
      // while the target is already visible.
      if (await seen(contactHeading, 1500)) {
        return
      }

      // Try to go to next page (use exact match to avoid matching "Next candidate" in modal)
      const nextButton = page.getByRole('button', { name: 'Next', exact: true })
      if (await seen(nextButton, 500)) {
        const isDisabled = await nextButton.isDisabled()
        if (!isDisabled) {
          await nextButton.click()
          continue
        }
      }

      // No more pages, contact not found on this attempt.
      break
    }

    await expect(contactHeading).toBeVisible({ timeout: 1000 })
    // Generous budget: when the shared pool hovers at the pagination
    // boundary, concurrent workers' seeding/cleanup can strand the client
    // on an emptied page several times in a row, and each recovery cycle
    // (reload + re-walk) costs a few seconds on the dev server.
  }).toPass({ timeout: 45000 })
}

/**
 * Same walk as findCandidateByName, minus the `page.reload()` recovery.
 *
 * A reload is a full navigation, which tears down the react-query cache. Any
 * spec whose subject IS the cache (does a mutation invalidate an already-cached
 * query?) goes vacuous the moment a reload happens — the remounted page
 * refetches from scratch and passes with or without the invalidation under
 * test. findCandidateByName's reload fires only on a retry attempt, so such a
 * spec passes locally and silently loses its meaning under parallel-worker
 * load.
 *
 * This variant rewinds by clicking the page-1 control when one exists, and
 * otherwise re-probes in place. If the candidate is genuinely unreachable the
 * toPass budget expires and the test fails loudly, which is the correct
 * outcome: a noisy failure beats a quiet vacuity.
 *
 * Deliberately a separate export. findCandidateByName keeps its reload — its
 * other callers rely on it for parallel-safety and do not care about the cache.
 */
export async function findCandidateByNameNoReload(
  page: Page,
  displayName: string,
  maxPages = 5
): Promise<void> {
  const contactHeading = page.getByRole('heading', { name: displayName, exact: true })
  const seen = (locator: Locator, timeout: number) =>
    locator
      .waitFor({ state: 'visible', timeout })
      .then(() => true)
      .catch(() => false)

  let attempt = 0
  await expect(async () => {
    attempt++
    if (attempt > 1) {
      // Rewind to page 1 if the control is present. When it is not, the pool
      // fits one page (nothing to rewind) or a mid-walk shrink stranded the
      // client with no rewind affordance — re-probe in place rather than
      // reload, and let the budget expire if the candidate never appears.
      const firstPageButton = page.getByRole('button', { name: '1', exact: true }).first()
      if (await seen(firstPageButton, 500)) {
        await firstPageButton.click()
      }
    }

    for (let i = 0; i < maxPages; i++) {
      if (await seen(contactHeading, 1500)) {
        return
      }

      // Exact match so "Next candidate" inside the resolver modal never matches.
      const nextButton = page.getByRole('button', { name: 'Next', exact: true })
      if (await seen(nextButton, 500)) {
        if (!(await nextButton.isDisabled())) {
          await nextButton.click()
          continue
        }
      }

      break
    }

    await expect(contactHeading).toBeVisible({ timeout: 1000 })
  }).toPass({ timeout: 45000 })
}
