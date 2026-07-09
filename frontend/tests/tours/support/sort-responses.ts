// Deterministic ordering for a capture's apiResponses, extracted from capture.ts
// so it is a pure, unit-testable function (the grader relies on stable ordering
// for before/after correlation, and a shuffled buffer must normalize identically).
//
// Pure — no Playwright import.

import type { ApiResponses } from './types'

// Emit apiResponses with a deterministic order: keys sorted, and each
// endpoint's items sorted by (method, requestUrl, status) — the buffer drains
// in response-completion order, which is non-deterministic run-to-run.
export function sortApiResponses(map: ApiResponses): ApiResponses {
  const sorted: ApiResponses = {}
  for (const key of Object.keys(map).sort()) {
    sorted[key] = [...map[key]].sort(
      (a, b) =>
        a.method.localeCompare(b.method) ||
        a.requestUrl.localeCompare(b.requestUrl) ||
        a.status - b.status
    )
  }
  return sorted
}
