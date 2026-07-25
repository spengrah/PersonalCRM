// pinned-fixtures.ts — resolving the seed's hand-authored tour fixtures.
//
// Every world state a tour depends on is pinned by the seed to a named contact
// carrying a MARKER inside its full_name. THE CONVENTION ITSELF — why a marker is
// a single lowercase alphanumeric token, why resolution must be exactly-one, and
// what each fixture guarantees — is documented once, at the seeding site:
// backend/internal/synthetic/fixtures.go. Do not restate it here; the literals
// below are the interface between the two, nothing more.
//
// Fixtures that a tour never resolves (the designated overdue contacts) are
// deliberately absent: those are population guarantees, and the tours that consume
// them select positionally because the selection is the behavior under test.

import type { APIRequestContext } from '@playwright/test'

export const FIXTURE_NO_ACTIVITY = 'fxnoactivity'
export const FIXTURE_OUTREACH = 'fxoutreach'
export const FIXTURE_RESPONSE = 'fxresponse'
export const FIXTURE_PENDING = 'fxpending'
export const FIXTURE_MERGE_TARGET = 'fxmergetarget'
export const FIXTURE_MERGE_SOURCE = 'fxmergesource'
export const FIXTURE_SEARCH = 'fxsearchsubject'
export const FIXTURE_DELETE = 'fxdeletevictim'
export const FIXTURE_BIRTHDAY = 'fxbirthday'

interface MarkedContact {
  id: string
  full_name: string
}

// resolveFixture returns the ONE contact carrying `marker`, via the same search
// endpoint a user's search box drives. Anything other than exactly one match is a
// seed problem, thrown loudly and named: touring on a mis-resolved subject
// produces evidence about the wrong contact, which reads as a real regression to
// anything grading it. Callers still verify the returned row's STATE before using
// it — a search that returned something is not proof it returned the right thing.
export async function resolveFixture<T extends MarkedContact>(
  apiCtx: APIRequestContext,
  marker: string,
  label: string
): Promise<T> {
  const resp = await apiCtx.get(`/api/v1/contacts?limit=200&search=${encodeURIComponent(marker)}`)
  const rows = ((await resp.json())?.data ?? []) as T[]
  const matches = rows.filter(c => c.full_name.includes(marker))
  if (matches.length !== 1) {
    throw new Error(
      `tour: pinned fixture "${marker}" (${label}) must resolve to exactly one contact, got ${matches.length} — ` +
        'the seed did not establish it, or a second contact carries the marker. Reseed before touring.'
    )
  }
  return matches[0]
}
