import type { Page, Request } from '@playwright/test'

/**
 * Negative assertions ("this request must NOT happen") have no positive signal
 * to await: a first-success poll passes immediately and would miss a late,
 * erroneous request. The only sound observation is a bounded window in which
 * the request would have appeared by now if the code were wrong.
 *
 * This file is the single place the `waitForTimeout` lint rule allows that
 * window (see the override in eslint.config.mjs). Specs call
 * `expectNoRequestWithin` instead of sleeping inline.
 */

/** Long enough for a delayed erroneous request to land; short enough to keep tests fast. */
export const NEGATIVE_ASSERTION_WINDOW_MS = 1500

/**
 * Fails if any request matching `matches` is issued within `windowMs`.
 * Start it BEFORE the action under test and await it AFTER, so the listener
 * is attached before the action can trigger a request.
 */
export async function expectNoRequestWithin(
  page: Page,
  matches: (request: Request) => boolean,
  windowMs: number = NEGATIVE_ASSERTION_WINDOW_MS
): Promise<void> {
  const seen: string[] = []
  const onRequest = (request: Request) => {
    if (matches(request)) {
      seen.push(`${request.method()} ${request.url()}`)
    }
  }
  page.on('request', onRequest)
  try {
    await page.waitForTimeout(windowMs)
  } finally {
    page.off('request', onRequest)
  }
  if (seen.length > 0) {
    throw new Error(
      `Expected no matching request within ${windowMs}ms, but saw ${seen.length}:\n${seen.join('\n')}`
    )
  }
}
