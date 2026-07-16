import { test, expect } from '@playwright/test'
import { expectAddContactHeader } from './helpers/dashboard'

test.describe('Error Boundary @area:error-boundary', () => {
  test('overdue loading shows placeholder skeletons, not an empty or caught-up state', async ({
    page,
  }) => {
    // spec: DSH-004[0], DSH-003[0]
    // Hold the overdue request open so the dashboard is pinned in its loading
    // state while we assert. The route is installed BEFORE goto (parallel-safe:
    // per-page interception, no DB mutation) and released afterwards so the
    // request does not dangle. The mock body carries the full apiClient
    // envelope ({ success, data }) — a bare body would silently misparse.
    let releaseOverdue: (() => void) | undefined
    const gate = new Promise<void>(resolve => {
      releaseOverdue = resolve
    })
    await page.route('**/api/v1/contacts/overdue*', async route => {
      await gate
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ success: true, data: [] }),
      })
    })

    await page.goto('/dashboard')
    // The header renders independently of the held widget.
    await expect(page.getByRole('heading', { name: 'Action Required', level: 2 })).toBeVisible()

    // While held (isLoading), assert the discriminating triple the old
    // verifier used: the labeled loading status present AND no caught-up
    // state AND no overdue cards. The held route guarantees the loading
    // state, so this cannot vacuously pass on a fast response.
    await expect(page.getByRole('status', { name: 'Loading overdue contacts' })).toBeVisible()
    await expect(page.getByText('All caught up')).toHaveCount(0)
    await expect(page.getByRole('button', { name: /Mark as Contacted/i })).toHaveCount(0)

    // The header add-contact CTA is available in the LOADING state too.
    await expectAddContactHeader(page)

    // Release the held request; the widget settles into the caught-up state,
    // proving the skeletons were the loading presentation, not a dead end.
    releaseOverdue?.()
    await expect(page.getByRole('heading', { name: /All caught up/ })).toBeVisible()
  })

  test('overdue failure shows an error state with a reason, not empty or caught-up', async ({
    page,
  }) => {
    // spec: DSH-004[1], DSH-004[2], DSH-003[0]
    // 500 the overdue endpoint (full apiClient failure envelope) BEFORE goto:
    // apiClient throws ApiError on !response.ok, React Query exhausts its
    // retries, and the dashboard renders its error branch.
    await page.route('**/api/v1/contacts/overdue*', route =>
      route.fulfill({
        status: 500,
        contentType: 'application/json',
        body: JSON.stringify({
          success: false,
          error: { code: 'INTERNAL_ERROR', message: 'Simulated overdue failure' },
        }),
      })
    )

    await page.goto('/dashboard')

    // React Query retries the failing query 3 times (~7s of backoff) before
    // surfacing the error, so allow a generous timeout. The error branch is a
    // role=alert block containing the heading and the reason.
    const alert = page.getByRole('alert')
    await expect(
      alert.getByRole('heading', { name: 'Error loading overdue contacts' })
    ).toBeVisible({ timeout: 20000 })

    // The shown reason FAITHFULLY reflects the actual failure (DSH-004[2]):
    // apiClient plumbs the envelope's error.message into ApiError.message and
    // the dashboard renders error.message, so the mocked failure message is
    // deterministically the rendered reason.
    await expect(alert.getByText('Simulated overdue failure')).toBeVisible()

    // The error state is distinct: no caught-up text, no overdue cards.
    await expect(page.getByText('All caught up')).toHaveCount(0)
    await expect(page.getByRole('button', { name: /Mark as Contacted/i })).toHaveCount(0)

    // The header add-contact CTA is available in the ERROR state too.
    await expectAddContactHeader(page)
  })
})
