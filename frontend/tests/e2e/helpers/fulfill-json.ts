import type { Route } from '@playwright/test'

// Fulfill a route with a JSON body. Playwright exempts fulfilled responses
// from CORS enforcement, so the app's cross-origin API calls (frontend :3000
// → API :8080, sent with an X-API-Key header) need no explicit CORS headers.
export function fulfillJson(route: Route, body: unknown, status = 200) {
  return route.fulfill({
    status,
    contentType: 'application/json',
    body: JSON.stringify(body),
  })
}
