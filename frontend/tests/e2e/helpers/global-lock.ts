/**
 * Cross-file, cross-worker mutex for shared DB-level singletons (e.g. the
 * mac_host table) that multiple spec files reset/reseed. Playwright workers
 * are separate OS processes, so an in-memory lock can't coordinate them —
 * and filesystem locks all share a stale-break TOCTOU under simultaneous
 * takeover. The arbiter is instead the test-only backend the suite already
 * talks to (POST /test/lock): one process, one mutex, no filesystem races,
 * and per-lane isolation for free since each E2E lane runs its own backend.
 *
 * The acquired lease expires unless renewed; a renew heartbeat runs while
 * the lock is held, so a SIGKILLed worker stops renewing and its lease
 * lapses instead of deadlocking the suite.
 *
 * Hold the lock once per FILE (beforeAll acquire, afterAll release), not
 * per test: per-test cycling lets the releasing worker instantly re-acquire
 * for its next serial test, starving the other file's waiter.
 */

const API_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || process.env.API_KEY || ''
const HEADERS = { 'X-API-Key': API_KEY, 'Content-Type': 'application/json' }

const LEASE_TTL_MS = 30_000
const RENEW_EVERY_MS = 10_000

export async function acquireGlobalLock(
  name: string,
  { deadlineMs = 300_000 }: { deadlineMs?: number } = {}
): Promise<() => Promise<void>> {
  const resp = await fetch(`${API_URL}/api/v1/test/lock`, {
    method: 'POST',
    headers: HEADERS,
    body: JSON.stringify({ name, wait_ms: deadlineMs, ttl_ms: LEASE_TTL_MS }),
  })
  if (!resp.ok) {
    throw new Error(
      `could not acquire global lock '${name}' within ${deadlineMs}ms ` +
        `(is the contending spec file stuck?): ${resp.status} ${await resp.text()}`
    )
  }
  const body = (await resp.json()) as { data: { lease_id: string } }
  const lease = body.data.lease_id

  // Heartbeat: a live holder keeps its lease fresh. One missed beat is
  // harmless (TTL is 3x the cadence), so transient renew failures are
  // logged and retried on the next tick. A 404 means the lease genuinely
  // lapsed and may have been taken over — mutual exclusion can no longer
  // be assumed, so record it and fail loudly at release time (throwing
  // from inside the interval would only surface as an unhandled rejection).
  let leaseLost: string | null = null
  const renewTimer = setInterval(() => {
    void fetch(`${API_URL}/api/v1/test/lock/${lease}/renew`, {
      method: 'POST',
      headers: HEADERS,
      body: JSON.stringify({ ttl_ms: LEASE_TTL_MS }),
    })
      .then(res => {
        if (res.status === 404) {
          leaseLost = `global lock '${name}' lease lapsed while held — mutual exclusion was not maintained`
          clearInterval(renewTimer)
        } else if (!res.ok) {
          console.error(`global lock '${name}': renew failed (${res.status}); retrying next beat`)
        }
      })
      .catch((err: unknown) => {
        console.error(`global lock '${name}': renew unreachable (${err}); retrying next beat`)
      })
  }, RENEW_EVERY_MS)
  // Do not keep the worker process alive just for the heartbeat.
  renewTimer.unref?.()

  let released = false
  return async () => {
    if (released) return
    released = true
    clearInterval(renewTimer)
    if (leaseLost) {
      throw new Error(leaseLost)
    }
    // Best-effort: a failed release just leaves the lease to lapse.
    await fetch(`${API_URL}/api/v1/test/lock/${lease}`, {
      method: 'DELETE',
      headers: HEADERS,
    }).catch(() => {})
  }
}
