import { describe, it, expect, beforeEach } from 'vitest'
import { spawn } from 'child_process'
import fs from 'fs'
import os from 'os'
import path from 'path'
import { acquireGlobalLock } from '../../../tests/e2e/helpers/global-lock'

// The lock coordinates separate Playwright worker PROCESSES, so the
// meaningful cases are cross-process: mutual exclusion under contention and
// takeover after a holder is killed without releasing. The suite lives under
// src/ because vitest excludes tests/e2e/**.

const LANE = `lock-test-${process.pid}`
const laneDir = () => path.join(os.tmpdir(), 'pcrm-e2e-locks', LANE)

beforeEach(() => {
  process.env.E2E_DATABASE_NAME = LANE
  fs.rmSync(laneDir(), { recursive: true, force: true })
})

/**
 * Spawn a plain-node child that takes the same lock via proper-lockfile
 * directly (same target path + stale settings as the wrapper) and holds it
 * until killed. Resolves with the child once it prints LOCKED.
 */
function spawnHolder(name: string, staleMs: number): Promise<ReturnType<typeof spawn>> {
  const lockfilePkg = path.resolve(__dirname, '../../../node_modules/proper-lockfile')
  const target = path.join(laneDir(), name)
  const script = `
    const fs = require('fs');
    const lockfile = require(${JSON.stringify(lockfilePkg)});
    fs.mkdirSync(${JSON.stringify(laneDir())}, { recursive: true });
    lockfile
      .lock(${JSON.stringify(target)}, { realpath: false, stale: ${staleMs} })
      .then(() => { console.log('LOCKED'); setInterval(() => {}, 1000); })
      .catch(err => { console.error(err.message); process.exit(1); });
  `
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, ['-e', script], { stdio: ['ignore', 'pipe', 'pipe'] })
    child.stdout?.on('data', chunk => {
      if (String(chunk).includes('LOCKED')) resolve(child)
    })
    child.stderr?.on('data', chunk => reject(new Error(`holder failed: ${chunk}`)))
    child.on('exit', code => reject(new Error(`holder exited early (${code})`)))
  })
}

describe('acquireGlobalLock', () => {
  it('blocks a second acquirer until release, then admits it', async () => {
    const release = await acquireGlobalLock('mutex', { staleMs: 60_000 })

    // While held (and heartbeating), a waiter with a short deadline times
    // out — the lock is never stolen from a live holder.
    await expect(
      acquireGlobalLock('mutex', { deadlineMs: 2_500, staleMs: 60_000 })
    ).rejects.toThrow(/could not acquire global lock 'mutex'/)

    await release()
    const release2 = await acquireGlobalLock('mutex', { deadlineMs: 5_000, staleMs: 60_000 })
    await release2()
  }, 20_000)

  it('release is idempotent', async () => {
    const release = await acquireGlobalLock('idem', { staleMs: 60_000 })
    await release()
    await expect(release()).resolves.toBeUndefined()
  })

  it('takes over from a holder process killed without releasing', async () => {
    const staleMs = 3_000
    const holder = await spawnHolder('crash', staleMs)
    holder.removeAllListeners('exit')
    holder.kill('SIGKILL')

    // The dead holder's heartbeat stops; takeover happens once the lock
    // goes stale. Deadline gives generous headroom over the stale window.
    const release = await acquireGlobalLock('crash', { deadlineMs: 15_000, staleMs })
    await release()
  }, 30_000)
})
