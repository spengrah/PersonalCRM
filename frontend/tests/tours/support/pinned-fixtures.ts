// pinned-fixtures.ts — resolving the seed's hand-authored tour fixtures.
//
// Every world state a tour depends on is pinned by the seed to a named contact
// carrying a MARKER inside its full_name. THE CONVENTION ITSELF — why a marker is
// a single lowercase alphanumeric token, why resolution must be exactly-one, and
// what each fixture guarantees — is documented once, at the seeding site:
// backend/internal/synthetic/fixtures.go. Do not restate it here; the literals
// below are the interface between the two, nothing more.
//
// No tour resolves the designated overdue contacts: those are population
// guarantees, and the tours that consume them select positionally because the
// selection is the behavior under test. What those tours DO need from here is the
// capture cap below, which is what carries a population guarantee through to the
// judge intact.
//
// These literals are hand-copied from Go constants, so pinned-fixtures.test.ts
// reads fixtures.go and fails on drift — a renamed marker would otherwise pass
// every per-PR gate and surface only as a resolveFixture throw on the nightly
// staging sweep, out-of-band and post-merge.

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

// The two designated overdue markers. Exported ONLY so the drift check covers the
// seed's full marker set — no tour resolves them, and none should: see above.
export const FIXTURE_OVERDUE_A = 'fxoverduea'
export const FIXTURE_OVERDUE_B = 'fxoverdueb'

// Every marker the seed declares, in no meaningful order. The drift check compares
// this against the Go constants as a SET, so a marker added on one side only is a
// failure rather than a silent divergence.
export const ALL_FIXTURE_MARKERS = [
  FIXTURE_NO_ACTIVITY,
  FIXTURE_OUTREACH,
  FIXTURE_RESPONSE,
  FIXTURE_PENDING,
  FIXTURE_MERGE_TARGET,
  FIXTURE_MERGE_SOURCE,
  FIXTURE_SEARCH,
  FIXTURE_DELETE,
  FIXTURE_BIRTHDAY,
  FIXTURE_OVERDUE_A,
  FIXTURE_OVERDUE_B,
]

// The overdue set is EVIDENCE, not a sample: the judge grades urgency ordering and
// tier separation across the WHOLE list, and the seed guarantees two designated
// overdue contacts are in it. A capture array longer than its cap is sliced
// head-first, so the tail is dropped — and under the name and last-contacted sorts
// that tail has nothing to do with urgency, which is how a designated fixture can
// end up in it. The prod-shaped overdue population measures 52 and the declared
// `standard` world 65, both above the 50-row default, so the overdue-bearing
// captures carry this EXPLICIT cap instead.
//
// This MUST equal synthetic.TourOverdueCaptureCap in
// backend/internal/synthetic/fixtures.go, which carries the reasoning behind the
// value; pinned-fixtures.test.ts reads that file and fails on drift in either
// direction.
export const OVERDUE_CAPTURE_CAP = 96

// Refuse to tour a world whose overdue population has outgrown the capture cap.
// Truncation is recorded in the capture but the dropped rows are not, so a judge
// grading a partial list cannot tell which contacts it never saw — the loud
// failure is the only outcome that stays honest.
export function assertOverdueFitsCapture(count: number, tourName: string): void {
  if (count > OVERDUE_CAPTURE_CAP) {
    throw new Error(
      `${tourName} tour: ${count} overdue contacts exceed the capture cap of ${OVERDUE_CAPTURE_CAP} — ` +
        'the captured overdue list would be truncated and the judge would grade a partial set. ' +
        'Bring the seeded overdue population back under the cap, or raise the cap deliberately.'
    )
  }
}

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
