import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'

// Cross-file, cross-worker mutex for shared DB-level singletons (e.g. the
// mac_host table) that multiple spec files reset/reseed. Playwright workers
// are separate OS processes, so an in-memory lock can't coordinate them --
// this uses an atomic `mkdir` as the lock primitive: mkdir either creates
// the directory or fails with EEXIST, so exactly one caller wins per name.

// A lock older than this is assumed to be abandoned by a crashed/killed
// worker rather than genuinely held, and is broken so the suite doesn't
// wedge forever on a stale lock.
const STALE_LOCK_MS = 5 * 60 * 1000
const POLL_INTERVAL_MS = 250

function sleep(ms: number): Promise<void> {
  return new Promise(resolve => setTimeout(resolve, ms))
}

// Locks are rooted under the E2E lane's database name so isolated lanes
// (their own DB, e.g. a scoped local run) never contend with each other or
// with a concurrently running default-lane suite.
function lockRoot(): string {
  const lane = process.env.E2E_DATABASE_NAME || 'personal_crm_test'
  return path.join(os.tmpdir(), 'pcrm-e2e-locks', lane)
}

/**
 * Acquires a named OS-level mutex, blocking (via a polling spin loop) until
 * it is free. Returns a release function -- call it, even on test failure,
 * to free the lock for the next waiter.
 */
export async function acquireGlobalLock(name: string): Promise<() => void> {
  const root = lockRoot()
  fs.mkdirSync(root, { recursive: true })
  const lockDir = path.join(root, `${name}.lock`)

  for (;;) {
    try {
      fs.mkdirSync(lockDir)
      break
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== 'EEXIST') {
        throw err
      }

      try {
        const stat = fs.statSync(lockDir)
        if (Date.now() - stat.mtimeMs > STALE_LOCK_MS) {
          // Break the stale lock and retry immediately -- whoever held it
          // is gone, so there's no live release to wait for.
          fs.rmdirSync(lockDir)
          continue
        }
      } catch {
        // The lock dir vanished between our failed mkdir and this stat --
        // the holder just released it. Retry the mkdir immediately.
        continue
      }

      await sleep(POLL_INTERVAL_MS)
    }
  }

  let released = false
  return () => {
    if (released) return
    released = true
    try {
      fs.rmdirSync(lockDir)
    } catch {
      // Best-effort: already gone (e.g. cleared by a stale-lock break) is fine.
    }
  }
}
