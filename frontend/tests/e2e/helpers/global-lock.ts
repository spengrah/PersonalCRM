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
const WATCHDOG_EVERY_MS = 1_000

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

  // Heartbeat + watchdog are deliberately SEPARATE timers. The heartbeat
  // only fires renews and records outcomes — it never throws, so nothing
  // in its promise chain can swallow an escalation. The watchdog is the
  // sole escalation path: it fires on its own cadence and, the moment the
  // lease is confirmed lapsed (renew 404) or simply hasn't been renewed
  // for a full TTL (repeated 5xx, a hung fetch, whatever the cause),
  // throws from a bare timer callback — an uncaught exception, which
  // Playwright fails the worker on. Escalation is thus independent of any
  // single renew's fate. One missed beat is harmless: the TTL is 3x the
  // cadence.
  let lastRenewOk = Date.now()
  let escalated = false
  let escalateTimer: ReturnType<typeof setTimeout> | undefined
  // The SINGLE escalation path, deliberately NOT inside the heartbeat's
  // promise chain (a throw there is caught by the trailing .catch()).
  // setTimeout(0) fires immediately as a bare timer callback — an uncaught
  // exception Playwright fails the worker on — so a confirmed lapse stops
  // the holder at once rather than at the next watchdog tick.
  const escalate = (why: string) => {
    if (escalated) return
    escalated = true
    clearInterval(renewTimer)
    clearInterval(watchdogTimer)
    escalateTimer = setTimeout(() => {
      throw new Error(
        `global lock '${name}' lost while held (${why}) — mutual exclusion can no longer be assumed`
      )
    }, 0)
    escalateTimer.unref?.()
  }
  const renewTimer = setInterval(() => {
    void fetch(`${API_URL}/api/v1/test/lock/${lease}/renew`, {
      method: 'POST',
      headers: HEADERS,
      body: JSON.stringify({ ttl_ms: LEASE_TTL_MS }),
    })
      .then(res => {
        if (res.status === 404) {
          escalate('lease lapsed')
        } else if (!res.ok) {
          console.error(`global lock '${name}': renew failed (${res.status}); retrying next beat`)
        } else {
          lastRenewOk = Date.now()
        }
      })
      .catch((err: unknown) => {
        console.error(`global lock '${name}': renew unreachable (${err}); retrying next beat`)
      })
  }, RENEW_EVERY_MS)
  // Independent of any single renew's fate: covers repeated 5xx and hung
  // fetches by escalating once no renew has landed for a full TTL. Polled
  // at 1s so detection is within ~1s of the TTL, not the renew cadence.
  const watchdogTimer = setInterval(() => {
    const staleFor = Date.now() - lastRenewOk
    if (staleFor > LEASE_TTL_MS) {
      escalate(`no renew for ${staleFor}ms`)
    }
  }, WATCHDOG_EVERY_MS)
  // Do not keep the worker process alive just for the timers.
  renewTimer.unref?.()
  watchdogTimer.unref?.()

  let released = false
  return async () => {
    if (released) return
    released = true
    clearInterval(renewTimer)
    clearInterval(watchdogTimer)
    if (escalateTimer) clearTimeout(escalateTimer)
    // Best-effort: a failed release just leaves the lease to lapse.
    await fetch(`${API_URL}/api/v1/test/lock/${lease}`, {
      method: 'DELETE',
      headers: HEADERS,
    }).catch(() => {})
  }
}
