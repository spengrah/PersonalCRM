// The tours test fixture. Extends @playwright/test with a per-test `tour` that
// bundles: a dedicated APIRequestContext carrying X-API-Key, a context-level
// /api/v1 response buffer attached BEFORE the first navigation (so no early
// response is missed), and the capture()/withDialog()/waitForApi()/holdRoute()
// helpers.
//
// Tours import ONLY `test` from here — never `expect` — so they stay
// assertion-free.

import { test as base, type Response } from '@playwright/test'
import { TourApi } from './capture'

export const test = base.extend<{ tour: TourApi }>({
  tour: async ({ page, playwright }, use, testInfo) => {
    const apiBase = process.env.TOURS_API_URL || process.env.TOURS_BASE_URL
    const apiKey = process.env.TOURS_API_KEY
    if (!apiBase || !apiKey) {
      throw new Error(
        'tours: TOURS_API_URL (or TOURS_BASE_URL) and TOURS_API_KEY must be set (see scripts/run-tours.sh)'
      )
    }

    const apiCtx = await playwright.request.newContext({
      baseURL: apiBase,
      extraHTTPHeaders: { 'X-API-Key': apiKey },
    })

    // Buffer every /api/v1 response since the last capture. Attached before the
    // test body runs (hence before its first navigation) so no early response
    // is missed.
    const buffer: Response[] = []
    const onResponse = (resp: Response): void => {
      if (resp.url().includes('/api/v1/')) buffer.push(resp)
    }
    page.context().on('response', onResponse)

    const tour = new TourApi(apiCtx, buffer, testInfo)
    try {
      await use(tour)
    } finally {
      page.context().off('response', onResponse)
      await apiCtx.dispose()
    }
  },
})
