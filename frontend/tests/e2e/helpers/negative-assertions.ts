import { errors, type Page, type Request } from '@playwright/test'

/**
 * Negative assertions ("this request must NOT happen") have no positive signal to
 * await. expectNoRequestDuring runs the action with a request listener attached,
 * then waits for a matching request for a bounded window after the action
 * returns: a matching request fails the test, and the wait timing out passes it.
 */

/** How long after the action a delayed erroneous request is still caught. */
export const NEGATIVE_ASSERTION_WINDOW_MS = 1500

export async function expectNoRequestDuring(
  page: Page,
  matches: (request: Request) => boolean,
  action: () => Promise<void>,
  windowMs: number = NEGATIVE_ASSERTION_WINDOW_MS
): Promise<void> {
  const seen: string[] = []
  const onRequest = (request: Request) => {
    if (matches(request)) seen.push(`${request.method()} ${request.url()}`)
  }
  page.on('request', onRequest)
  try {
    await action()
    if (seen.length === 0) {
      const late = await page
        .waitForEvent('request', { predicate: matches, timeout: windowMs })
        .catch(error => {
          if (error instanceof errors.TimeoutError) return null
          throw error
        })
      if (late) seen.push(`${late.method()} ${late.url()}`)
    }
  } finally {
    page.off('request', onRequest)
  }
  if (seen.length > 0) {
    throw new Error(
      `Expected no matching request during the action or ${windowMs}ms after it, but saw:\n${seen.join('\n')}`
    )
  }
}
