import { describe, it, expect } from 'vitest'
import fs from 'fs'
import path from 'path'
import {
  ALL_FIXTURE_MARKERS,
  OVERDUE_CAPTURE_CAP,
  assertOverdueFitsCapture,
  resolveFixture,
} from './pinned-fixtures'
import { DEFAULT_ARRAY_CAP } from './normalize'
import type { APIRequestContext } from '@playwright/test'

// The tour-side fixture machinery is a set of fail-loud gates, and a gate nobody
// has watched fail is indistinguishable from one that always passes. These are the
// bad inputs each gate exists to reject.

const FIXTURES_GO = path.join(
  import.meta.dirname ?? __dirname,
  '..',
  '..',
  '..',
  '..',
  'backend',
  'internal',
  'synthetic',
  'fixtures.go'
)

// A stub standing in for Playwright's APIRequestContext: only `get` is reached,
// and only its JSON body matters here.
function stubApiCtx(rows: Array<{ id: string; full_name: string }>): APIRequestContext {
  return {
    get: async () => ({ json: async () => ({ data: rows }) }),
  } as unknown as APIRequestContext
}

describe('assertOverdueFitsCapture', () => {
  it('throws above the cap, naming the tour and the count', () => {
    expect(() => assertOverdueFitsCapture(OVERDUE_CAPTURE_CAP + 1, 'dashboard')).toThrow(
      new RegExp(
        `dashboard tour: ${OVERDUE_CAPTURE_CAP + 1} overdue contacts exceed the capture cap of ${OVERDUE_CAPTURE_CAP}`
      )
    )
  })

  it('accepts a population exactly at the cap', () => {
    expect(() => assertOverdueFitsCapture(OVERDUE_CAPTURE_CAP, 'dashboard')).not.toThrow()
  })

  it('accepts the population the seed actually ships', () => {
    // The guard must not fire on the shipping seed — a cap that rejects the
    // intended population is as broken as one that never fires. The declared
    // `standard` world's overdue population is measured at 65.
    expect(() => assertOverdueFitsCapture(65, 'relationship-loop')).not.toThrow()
  })

  it('leaves headroom over the default array cap', () => {
    // The whole point of the explicit cap: at DEFAULT_ARRAY_CAP the shipping
    // population truncates.
    expect(OVERDUE_CAPTURE_CAP).toBeGreaterThan(DEFAULT_ARRAY_CAP)
  })
})

describe('resolveFixture', () => {
  const row = (id: string, full_name: string) => ({ id, full_name })

  it('returns the single marker-bearing row', async () => {
    const ctx = stubApiCtx([row('a', 'synth-ns-Zeta Testwell fxsearchsubject')])
    await expect(resolveFixture(ctx, 'fxsearchsubject', 'label')).resolves.toMatchObject({
      id: 'a',
    })
  })

  it('throws when nothing carries the marker', async () => {
    const ctx = stubApiCtx([])
    await expect(resolveFixture(ctx, 'fxsearchsubject', 'CON-065')).rejects.toThrow(
      /must resolve to exactly one contact, got 0/
    )
  })

  it('throws when two contacts carry the marker', async () => {
    const ctx = stubApiCtx([
      row('a', 'synth-ns-Zeta Testwell fxsearchsubject'),
      row('b', 'synth-ns-Vex Mockford fxsearchsubject'),
    ])
    await expect(resolveFixture(ctx, 'fxsearchsubject', 'CON-065')).rejects.toThrow(
      /must resolve to exactly one contact, got 2/
    )
  })

  it('ignores search hits that do not carry the marker', async () => {
    // Full-text search ranks and returns neighbours; only the marker decides.
    const ctx = stubApiCtx([
      row('a', 'synth-ns-Zeta Testwell'),
      row('b', 'synth-ns-Vex Mockford fxsearchsubject'),
    ])
    await expect(resolveFixture(ctx, 'fxsearchsubject', 'CON-065')).resolves.toMatchObject({
      id: 'b',
    })
  })
})

describe('marker parity with the Go seed', () => {
  // The markers here are hand-copied across the language boundary. A Go-side
  // rename passes lint, unit, integration and E2E, and fails only as a
  // resolveFixture throw on the nightly staging sweep — this pulls that into the
  // deterministic lane.
  const goMarkers = (): string[] => {
    const src = fs.readFileSync(FIXTURES_GO, 'utf8')
    return [...src.matchAll(/^\tFixtureMarker\w+\s*=\s*"([a-z0-9]+)"$/gm)].map(m => m[1])
  }

  it('finds the Go constants at all (the regex is load-bearing)', () => {
    expect(goMarkers().length).toBeGreaterThan(0)
  })

  it('declares exactly the markers the seed declares', () => {
    expect([...ALL_FIXTURE_MARKERS].sort()).toEqual([...goMarkers()].sort())
  })
})

describe('overdue capture cap parity with the Go seed', () => {
  // The cap is a two-sided contract: the Go side asserts the seeded world stays
  // under it, the TS side refuses to tour a world that exceeds it. Hand-copied
  // across the language boundary, so a one-sided change would either let a
  // truncated list reach the judge or stop a perfectly good world from touring.
  const goCap = (): number | null => {
    const src = fs.readFileSync(FIXTURES_GO, 'utf8')
    const m = src.match(/^const TourOverdueCaptureCap = (\d+)$/m)
    return m ? Number(m[1]) : null
  }

  it('finds the Go constant at all (the regex is load-bearing)', () => {
    expect(goCap()).not.toBeNull()
  })

  it('equals synthetic.TourOverdueCaptureCap', () => {
    expect(OVERDUE_CAPTURE_CAP).toBe(goCap())
  })
})
