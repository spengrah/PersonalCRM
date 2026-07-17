import fs from 'fs'
import os from 'os'
import path from 'path'
import lockfile from 'proper-lockfile'

/**
 * Cross-file, cross-worker mutex for shared DB-level singletons (e.g. the
 * mac_host table) that multiple spec files reset/reseed. Playwright workers
 * are separate OS processes, so an in-memory lock can't coordinate them.
 *
 * Built on proper-lockfile rather than a hand-rolled protocol: it acquires
 * atomically (mkdir), heartbeats the lock's mtime while the holder process
 * is alive — so a slow-but-alive holder is never stolen — and allows
 * takeover only after the heartbeat has stopped (`stale`), which is what a
 * crashed worker looks like. Holders also release via the library's
 * process-exit hook, so no reaper logic lives here.
 *
 * Hold the lock once per FILE (beforeAll acquire, afterAll release), not
 * per test: per-test cycling lets the releasing worker instantly re-acquire
 * for its next serial test, starving waiters that only poll between holds.
 *
 * Locks are keyed per E2E lane (E2E_DATABASE_NAME) so isolated lanes on one
 * machine never contend with each other or with a default-lane run.
 */
export async function acquireGlobalLock(
  name: string,
  { deadlineMs = 300_000, staleMs = 15_000 }: { deadlineMs?: number; staleMs?: number } = {}
): Promise<() => Promise<void>> {
  const laneDir = path.join(
    os.tmpdir(),
    'pcrm-e2e-locks',
    process.env.E2E_DATABASE_NAME || 'personal_crm_test'
  )
  fs.mkdirSync(laneDir, { recursive: true })
  const target = path.join(laneDir, name)

  const retryEveryMs = 1_000
  try {
    const release = await lockfile.lock(target, {
      // The lock target is a name under the lane dir, not an existing file.
      realpath: false,
      // A holder whose process died stops heartbeating (mtime updates at
      // stale/2) and can be taken over after this long.
      stale: staleMs,
      retries: {
        retries: Math.ceil(deadlineMs / retryEveryMs),
        factor: 1,
        minTimeout: retryEveryMs,
        maxTimeout: retryEveryMs,
      },
      // Our hold was broken underneath us (heartbeat stalled long enough
      // for a takeover). Continuing would mean two holders mutating the
      // singleton — crash the worker loudly instead.
      onCompromised: err => {
        throw new Error(`global lock '${name}' compromised while held: ${err.message}`)
      },
    })
    // Idempotent release: afterAll and the library's exit hook can both
    // fire; the second call must be a no-op, not an ERELEASED throw.
    let released = false
    return async () => {
      if (released) return
      released = true
      await release()
    }
  } catch (err) {
    throw new Error(
      `could not acquire global lock '${name}' within ${deadlineMs}ms ` +
        `(is the contending spec file stuck?): ${err instanceof Error ? err.message : String(err)}`
    )
  }
}
