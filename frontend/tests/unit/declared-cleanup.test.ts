import { describe, expect, it, vi, afterEach } from 'vitest'
import type { APIRequestContext, TestInfo } from '@playwright/test'

import { createTestAPI, type NamespaceCleanupOutcome, type TestAPI } from '../e2e/helpers/test-api'

/**
 * The declared-namespace cleanup path in the E2E helper, tested without a
 * server.
 *
 * This is the surface every migrated E2E spec depends on for isolation: it is
 * the ONLY thing standing between a lost seed response and synthetic rows
 * living on in the shared E2E database. Both properties below are invisible
 * from the E2E suite itself — they only show up as another test's flake — so
 * they are pinned here.
 */

type CleanupResults = Record<string, NamespaceCleanupOutcome>
type CleanupStep = (sent: string[]) => CleanupResults

interface Harness {
  api: TestAPI
  /** The `namespaces` list of each declared-cleanup POST, in order. */
  cleanupCalls: string[][]
  /** Every value passed to testInfo.setTimeout. */
  timeoutGrants: number[]
}

interface HarnessOptions {
  /** Makes the prefix-shape cleanup fail, as a transient 500 would. */
  prefixCleanup?: 'ok' | 'fails'
}

const TEST_TIMEOUT_MS = 30_000

function jsonResponse(status: number, payload: unknown) {
  const body = JSON.stringify(payload)
  return {
    ok: () => status >= 200 && status < 300,
    status: () => status,
    text: async () => body,
    json: async () => JSON.parse(body),
  }
}

/**
 * A TestAPI wired to a scripted server. `steps` answers the declared-cleanup
 * POSTs in order; the last step repeats for any further attempt, which is what
 * lets a test model "still busy forever".
 */
function harness(steps: CleanupStep[], harnessOptions: HarnessOptions = {}): Harness {
  const cleanupCalls: string[][] = []
  const timeoutGrants: number[] = []

  const request = {
    post: async (url: string, options: { data: Record<string, unknown> }) => {
      if (url.endsWith('/api/v1/test/seed/declared')) {
        return jsonResponse(201, {
          success: true,
          data: { namespace: options.data.namespace, anchor: '2026-01-01T00:00:00Z', entities: {} },
        })
      }
      if (url.endsWith('/api/v1/test/cleanup')) {
        const namespaces = options.data.namespaces as string[] | undefined
        if (!namespaces) {
          // The prefix shape cleanup() issues first.
          if (harnessOptions.prefixCleanup === 'fails') {
            return jsonResponse(500, {
              success: false,
              error: { message: 'prefix sweep exploded' },
            })
          }
          return jsonResponse(200, {
            success: true,
            data: { deleted_contacts: 0 },
          })
        }
        cleanupCalls.push(namespaces)
        const step = steps[Math.min(cleanupCalls.length - 1, steps.length - 1)]
        return jsonResponse(200, {
          success: true,
          data: { expansions: {}, results: step(namespaces) },
        })
      }
      throw new Error(`unexpected POST ${url}`)
    },
  } as unknown as APIRequestContext

  const testInfo: Pick<TestInfo, 'workerIndex' | 'timeout' | 'setTimeout'> = {
    workerIndex: 0,
    timeout: TEST_TIMEOUT_MS,
    setTimeout(ms: number) {
      timeoutGrants.push(ms)
      this.timeout = ms
    },
  }

  return { api: createTestAPI(request, testInfo as TestInfo), cleanupCalls, timeoutGrants }
}

const all =
  (status: NamespaceCleanupOutcome['status']): CleanupStep =>
  sent =>
    Object.fromEntries(sent.map(namespace => [namespace, { status }])) as CleanupResults

/** Runs cleanup() to completion under fake timers, returning its outcome. */
async function runCleanup(api: TestAPI): Promise<Error | 'resolved'> {
  const settled = api.cleanup().then(
    () => 'resolved' as const,
    (error: Error) => error
  )
  // Comfortably past the poll budget, so the loop either finishes or gives up.
  await vi.advanceTimersByTimeAsync(300_000)
  return settled
}

afterEach(() => {
  vi.useRealTimers()
})

describe('declared namespace cleanup', () => {
  /**
   * A seed whose response was lost keeps running server-side for up to the run
   * budget, holding the reservation that makes cleanup answer `busy`. A client
   * that gave up after one retry would walk away from rows it created.
   */
  it('keeps polling a busy namespace well past the first retry', async () => {
    vi.useFakeTimers()
    const busyAttempts = 20
    const { api, cleanupCalls, timeoutGrants } = harness([
      sent => (cleanupCalls.length <= busyAttempts ? all('busy')(sent) : all('cleaned')(sent)),
    ])
    await api.seedBehavior('DSH-005')

    await expect(runCleanup(api)).resolves.toBe('resolved')
    expect(cleanupCalls.length).toBe(busyAttempts + 1)
    // The poll outlasts the run only if the hook is given the wall-clock for
    // it: afterEach shares the test's timeout slot.
    expect(timeoutGrants).toEqual([TEST_TIMEOUT_MS + 200_000])
  })

  /** The bound is real: a namespace that never clears fails loudly. */
  it('gives up after the budget and names the namespace it could not clean', async () => {
    vi.useFakeTimers()
    const { api, cleanupCalls } = harness([all('busy')])
    const seeded = await api.seedBehavior('DSH-005')

    const outcome = await runCleanup(api)
    expect(outcome).toBeInstanceOf(Error)
    expect((outcome as Error).message).toContain(seeded.namespace)
    expect((outcome as Error).message).toContain('busy')
    // 200s budget / 2s interval — bounded, and far more than the single retry
    // it used to make.
    expect(cleanupCalls.length).toBeGreaterThan(90)
    expect(cleanupCalls.length).toBeLessThan(110)
  })

  /**
   * A retry names only the retriable namespaces, so a non-retriable `error` is
   * absent from every later response. Reporting the last response alone would
   * call the whole cleanup a success while that namespace's rows are still
   * there.
   */
  it('preserves an error from the first attempt when a later namespace clears', async () => {
    vi.useFakeTimers()
    const { api } = harness([
      sent =>
        Object.fromEntries(
          sent.map((namespace, index) => [
            namespace,
            index === 0
              ? { status: 'error' as const, error: 'descendant guard' }
              : { status: 'busy' as const },
          ])
        ),
      all('cleaned'),
    ])
    const first = await api.seedBehavior('DSH-005')
    const second = await api.seedBehavior('CAD-026')

    const outcome = await runCleanup(api)
    expect(outcome).toBeInstanceOf(Error)
    expect((outcome as Error).message).toContain(first.namespace)
    expect((outcome as Error).message).toContain('descendant guard')
    expect((outcome as Error).message).not.toContain(`"${second.namespace}"`)
  })

  /**
   * The server caps one cleanup request at 32 namespaces and rejects an
   * oversized list with a 400 BEFORE deleting anything — so a single oversized
   * request would strand every declared world the test seeded, not just the
   * ones past the cap. A test reaches the cap at 32 seeds, or at 16 once each
   * seed records both its requested and its re-salted name.
   */
  it('splits an over-cap namespace list into requests the server accepts', async () => {
    vi.useFakeTimers()
    const seeds = 40
    const { api, cleanupCalls } = harness([all('cleaned')])
    for (let i = 0; i < seeds; i++) await api.seedBehavior('DSH-005')

    await expect(runCleanup(api)).resolves.toBe('resolved')

    expect(cleanupCalls.length).toBe(2)
    for (const sent of cleanupCalls) expect(sent.length).toBeLessThanOrEqual(32)
    // Every namespace is still asked about exactly once: batching must split
    // the work, not drop or duplicate any of it.
    const sent = cleanupCalls.flat()
    expect(sent.length).toBe(seeds)
    expect(new Set(sent).size).toBe(seeds)
  })

  /**
   * Expansion can move a token's verdict to a different key between attempts:
   * an in-flight run answers under the REQUESTED token, and under `<ns>-sN`
   * once it finishes and the salted world becomes discoverable. Carrying the
   * superseded entry forward would report a phantom failure for a world that
   * was in fact swept.
   */
  it('supersedes a re-sent token whose verdict moves to its salted variant', async () => {
    vi.useFakeTimers()
    const { api } = harness([
      all('busy'),
      sent => Object.fromEntries(sent.map(namespace => [`${namespace}-s1`, { status: 'cleaned' }])),
    ])
    await api.seedBehavior('DSH-005')

    await expect(runCleanup(api)).resolves.toBe('resolved')
  })

  /**
   * The two sweeps delete disjoint rows by unrelated mechanisms, so a failure of
   * the prefix one says nothing about the declared worlds. Returning
   * early on it would leave every declared world alive in the shared E2E
   * database, turning one test's transient 500 into an isolation failure for
   * everything that runs afterwards.
   */
  it('cleans declared namespaces even when the prefix sweep fails, and reports both', async () => {
    vi.useFakeTimers()
    const { api, cleanupCalls } = harness([all('cleaned')], { prefixCleanup: 'fails' })
    const seeded = await api.seedBehavior('DSH-005')

    const outcome = await runCleanup(api)
    expect(outcome).toBeInstanceOf(Error)
    // The prefix failure is reported...
    expect((outcome as Error).message).toContain('Failed to cleanup test data')
    // ...and the declared sweep still ran, for the namespace it seeded.
    expect(cleanupCalls).toEqual([[seeded.namespace]])
  })

  /**
   * The mirror case: a prefix failure must not swallow a declared failure
   * either. Both errors reach the caller, because either one alone leaves rows.
   */
  it('reports the declared failure alongside the prefix failure', async () => {
    vi.useFakeTimers()
    const { api } = harness([all('error')], { prefixCleanup: 'fails' })
    const seeded = await api.seedBehavior('DSH-005')

    const outcome = await runCleanup(api)
    expect(outcome).toBeInstanceOf(Error)
    expect((outcome as Error).message).toContain('Failed to cleanup test data')
    expect((outcome as Error).message).toContain(seeded.namespace)
  })
})
