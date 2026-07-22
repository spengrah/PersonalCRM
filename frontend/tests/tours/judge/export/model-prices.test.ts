// Tests for the model-price sync.
//
// The write path is unreachable from a read-only instance, so every assertion here
// runs against a fake transport that records its calls IN ORDER — which is what the
// dangerous properties are about: that a delete precedes a create, that the
// effective override is deleted LAST, that a failure stops rather than continues.
//
// The fake speaks shapes this code does NOT assume as well as ones it does (a bare
// health body, a legacy flat-priced row, rows carrying upstream's server-owned tier
// ids, an Authorization-bearing request), so the suite can falsify the assumptions
// rather than agree with them.

import { describe, expect, it, vi } from 'vitest'
import {
  AmbiguousMatchError,
  AmbiguousTieError,
  InstanceShapeError,
  NotFoundUpstreamError,
  PatternError,
  UpstreamShapeError,
  blockingOverrides,
  buildCreateBody,
  decideAction,
  diffPrices,
  effectiveWithin,
  fetchUpstream,
  guardDelta,
  guardDeltasFor,
  main,
  orderedForDelete,
  parseInstanceRow,
  parseUpstream,
  selectEffective,
  selectUpstream,
  splitInstanceRows,
  toJsRegex,
  type InstanceModel,
  type UpstreamModel,
  type UpstreamTier,
} from './model-prices'
import {
  AMBIGUOUS_SET,
  DECOY,
  UNIQUE_RAW,
  UNIQUE_SET,
  sampleEntry,
} from './fixtures/upstream-model-prices'
import * as crypto from 'crypto'

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const JUDGE = 'gpt-5.4-mini'
const INTENT = 'gpt-5.5'
const LUNA = 'gpt-5.6-luna'

const ENV = {
  LANGFUSE_HOST: 'http://lf',
  LANGFUSE_PUBLIC_KEY: 'pk',
  LANGFUSE_SECRET_KEY: 'sk',
}

const FAST = { upstreamMs: 20, apiMs: 20, healthMs: 20 }

const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v)) as T

/** An instance row built from a VERBATIM upstream entry, so "matches upstream" is a
 * real match rather than a fixture shaped to agree. Carries the deprecated flat
 * `prices` view (deliberately disagreeing with the tiers) and `unit: null`, both of
 * which the diff must ignore. */
function row(
  entry: Record<string, unknown>,
  over: Record<string, unknown> = {}
): Record<string, unknown> {
  return {
    id: `row-${String(entry.modelName)}`,
    modelName: entry.modelName,
    matchPattern: entry.matchPattern,
    isLangfuseManaged: true,
    startDate: null,
    unit: null,
    // The deprecated flattened view; its shape differs from the tier price maps.
    prices: { input: { price: 99 }, output: { price: 99 } },
    inputPrice: 99,
    outputPrice: 99,
    // Verbatim tiers, INCLUDING upstream's server-owned tier ids.
    pricingTiers: clone(entry.pricingTiers),
    ...over,
  }
}

/** The same row with its default tier's input price doubled — drift. */
function stale(entry: Record<string, unknown>, over: Record<string, unknown> = {}) {
  const tiers = clone(entry.pricingTiers) as Array<{ prices: Record<string, number> }>
  tiers[0].prices.input *= 2
  return row(entry, { pricingTiers: tiers, ...over })
}

const asInstance = (raw: Record<string, unknown>): InstanceModel => parseInstanceRow(raw, 'test')

const upstreamOf = (name: string): UpstreamModel =>
  parseUpstream(JSON.stringify([sampleEntry(name)]))[0]

// ---------------------------------------------------------------------------
// Fake transport
// ---------------------------------------------------------------------------

interface MockOpts {
  models?: Record<string, unknown>[]
  perPage?: number
  upstreamText?: string
  upstreamStatus?: number
  upstreamNeverSettles?: boolean
  /** 1-based page index whose response never settles (bounds-the-walk test). */
  pageNeverSettles?: number
  /** 1-based index of a models LIST request (page 1) that fails — a re-read error. */
  failListCall?: number
  failDelete?: (id: string, nth: number) => boolean
  failCreate?: boolean
  health?: Record<string, unknown> | 'error'
}

function mockTransport(opts: MockOpts = {}) {
  const upstreamRequests: Array<{ url: string; headerKeys: string[] }> = []
  const langfuseRequests: Array<{ path: string; method: string; headerKeys: string[] }> = []
  const deletes: string[] = []
  const creates: Record<string, unknown>[] = []
  // LIVE instance state, not a fixed reply: a DELETE removes the row and a POST adds
  // one, so a second read within the same run sees what the first write did. A list
  // endpoint frozen at the initial fixture would make any re-read logic look correct
  // by construction. The array is copied so a run cannot mutate the caller's fixture.
  const liveRows: Record<string, unknown>[] = [...(opts.models ?? [])]
  let created = 0
  // The REAL create route is create-only and rejects on (project, modelName)
  // uniqueness — the pattern is not part of the key. Modelled here so a write test
  // that leaves a same-named project row behind fails the way the server would,
  // instead of against a double that happily accepts duplicates.
  const liveProjectNames = (): string[] =>
    liveRows.filter(r => r.isLangfuseManaged === false).map(r => String(r.modelName))
  /** Every write, in order — 'delete <id>' / 'create <modelName>'. */
  const order: string[] = []
  let deleteCount = 0
  let listCalls = 0

  const okText = (obj: unknown): Response =>
    ({ ok: true, status: 200, text: async () => JSON.stringify(obj) }) as Response
  const errRes = (status: number, msg: string): Response =>
    ({ ok: false, status, text: async () => msg }) as Response
  const stall = (signal: AbortSignal | undefined): Promise<Response> =>
    new Promise<Response>((_resolve, reject) => {
      // A real aborted fetch rejects with an AbortError-NAMED error; anything that
      // classifies by name must see the faithful shape.
      signal?.addEventListener('abort', () => {
        const e = new Error('The operation was aborted')
        e.name = 'AbortError'
        reject(e)
      })
    })

  const fetchImpl = (async (
    url: string | URL,
    init?: {
      method?: string
      body?: unknown
      signal?: AbortSignal
      headers?: Record<string, string>
    }
  ) => {
    const u = String(url)
    const method = init?.method ?? 'GET'
    const headerKeys = Object.keys(init?.headers ?? {})
    if (u.startsWith('https://raw.githubusercontent.com')) {
      upstreamRequests.push({ url: u, headerKeys })
      if (opts.upstreamNeverSettles === true) return stall(init?.signal)
      const status = opts.upstreamStatus ?? 200
      const body = opts.upstreamText ?? UNIQUE_RAW
      return { ok: status >= 200 && status < 300, status, text: async () => body } as Response
    }
    const parsed = new URL(u)
    const pathname = parsed.pathname
    langfuseRequests.push({ path: pathname, method, headerKeys })

    if (pathname === '/api/public/health') {
      if (opts.health === 'error') return errRes(500, 'health boom')
      return okText(opts.health ?? { status: 'OK', version: '3.212.0' })
    }
    if (pathname === '/api/public/models' && method === 'GET') {
      const page0 = Number(parsed.searchParams.get('page') ?? '1')
      if (page0 === 1) {
        listCalls++
        if (opts.failListCall === listCalls) return errRes(500, 'models list boom')
      }
      const all = liveRows
      const per = opts.perPage ?? 100
      const page = Number(parsed.searchParams.get('page') ?? '1')
      if (opts.pageNeverSettles === page) return stall(init?.signal)
      const totalPages = Math.max(1, Math.ceil(all.length / per))
      return okText({
        data: all.slice((page - 1) * per, page * per),
        meta: { page, limit: per, totalItems: all.length, totalPages },
      })
    }
    if (pathname === '/api/public/models' && method === 'POST') {
      const body = JSON.parse(String(init?.body)) as Record<string, unknown>
      if (opts.failCreate === true) {
        order.push(`create-failed ${String(body.modelName)}`)
        return errRes(500, 'create boom')
      }
      if (liveProjectNames().includes(String(body.modelName))) {
        order.push(`create-409 ${String(body.modelName)}`)
        return errRes(400, `Model name '${String(body.modelName)}' already exists in project`)
      }
      created++
      liveRows.push({
        ...body,
        id: `new-${created}`,
        isLangfuseManaged: false,
        startDate: null,
      })
      creates.push(body)
      order.push(`create ${String(body.modelName)}`)
      return okText({ id: `new-${creates.length}` })
    }
    const del = /^\/api\/public\/models\/(.+)$/.exec(pathname)
    if (del !== null && method === 'DELETE') {
      const id = decodeURIComponent(del[1])
      deleteCount++
      if (opts.failDelete?.(id, deleteCount) === true) {
        order.push(`delete-failed ${id}`)
        return errRes(500, 'delete boom')
      }
      deletes.push(id)
      const at = liveRows.findIndex(r => String(r.id) === id)
      if (at >= 0) liveRows.splice(at, 1)
      order.push(`delete ${id}`)
      return okText({})
    }
    return errRes(404, `unexpected ${method} ${pathname}`)
  }) as unknown as typeof fetch

  return { fetchImpl, upstreamRequests, langfuseRequests, deletes, creates, order }
}

interface RunResult {
  code: number
  out: string[]
  err: string[]
  mock: ReturnType<typeof mockTransport>
}

async function run(
  argv: string[],
  mockOpts: MockOpts = {},
  env: Record<string, string | undefined> = ENV
): Promise<RunResult> {
  const mock = mockTransport(mockOpts)
  vi.stubGlobal('fetch', mock.fetchImpl)
  const out: string[] = []
  const err: string[] = []
  try {
    const code = await main(argv, env, {
      fetchImpl: mock.fetchImpl,
      log: s => out.push(s),
      errlog: s => err.push(s),
      timeouts: FAST,
    })
    return { code, out, err, mock }
  } finally {
    vi.unstubAllGlobals()
  }
}

/** The `action=` word reported for one target. */
const actionFor = (out: string[], target: string): string | undefined => {
  const line = out.find(l => l.startsWith(`model=${target} action=`))
  return line === undefined ? undefined : /action=(\S+)/.exec(line)?.[1]
}
const lineFor = (out: string[], target: string): string =>
  out.find(l => l.startsWith(`model=${target} action=`)) ?? ''

// ===========================================================================
// Reconciliation — the five states
// ===========================================================================

describe('reconciliation', () => {
  it('row 1 — managed matches upstream and no override exists: ZERO writes', async () => {
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', JUDGE, '--strict'], { models: [row(entry)] })
    expect(actionFor(r.out, JUDGE)).toBe('none')
    expect(r.mock.creates).toHaveLength(0)
    expect(r.mock.deletes).toHaveLength(0)
    expect(r.code).toBe(0)
  })

  it('row 2 — managed has caught up while our override stands: DELETE it, create nothing', async () => {
    // Only ever fires after a real Langfuse image upgrade, so it can never be
    // exercised by accident — it has to be a test.
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [row(entry), row(entry, { id: 'ovr-1', isLangfuseManaged: false })],
    })
    expect(actionFor(r.out, JUDGE)).toBe('delete')
    expect(r.mock.deletes).toEqual(['ovr-1'])
    expect(r.mock.creates).toHaveLength(0)
    expect(r.code).toBe(0)
  })

  it('row 3 — managed stale, no override: one POST, no DELETE, managed row untouched', async () => {
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', JUDGE, '--strict'], { models: [stale(entry)] })
    expect(actionFor(r.out, JUDGE)).toBe('create')
    expect(r.mock.creates).toHaveLength(1)
    expect(r.mock.deletes).toHaveLength(0)
    expect(r.mock.order).toEqual([`create ${String(entry.modelName)}`])
  })

  it('row 4 — managed stale and the override differs: DELETE precedes POST', async () => {
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [stale(entry), stale(entry, { id: 'ovr-1', isLangfuseManaged: false })],
    })
    expect(actionFor(r.out, JUDGE)).toBe('replace')
    expect(r.mock.order).toEqual(['delete ovr-1', `create ${String(entry.modelName)}`])
  })

  it('row 5 — managed stale but the override already matches upstream: ZERO writes', async () => {
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [stale(entry), row(entry, { id: 'ovr-1', isLangfuseManaged: false })],
    })
    expect(actionFor(r.out, JUDGE)).toBe('none')
    expect(lineFor(r.out, JUDGE)).toContain('override already matches upstream')
    expect(r.mock.order).toEqual([])
  })

  it('deletes EVERY matching override before creating, and reports the multiplicity', async () => {
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [
        stale(entry),
        stale(entry, { id: 'ovr-1', isLangfuseManaged: false }),
        stale(entry, { id: 'ovr-2', isLangfuseManaged: false }),
      ],
    })
    expect(r.mock.deletes.sort()).toEqual(['ovr-1', 'ovr-2'])
    expect(r.mock.order[2]).toBe(`create ${String(entry.modelName)}`)
    expect(r.out.some(l => l.includes('note=2 project-scoped override rows'))).toBe(true)
  })

  it('a DATED override cannot survive a replace and outrank the new undated row', async () => {
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [
        stale(entry),
        stale(entry, {
          id: 'ovr-dated',
          isLangfuseManaged: false,
          startDate: '2026-01-01T00:00:00Z',
        }),
        stale(entry, { id: 'ovr-null', isLangfuseManaged: false }),
      ],
    })
    expect(r.mock.deletes).toContain('ovr-dated')
    expect(r.mock.deletes).toContain('ovr-null')
    expect(r.mock.creates).toHaveLength(1)
  })

  it('plural managed rows — the EFFECTIVE one decides, not "any of them matches"', async () => {
    // The older managed row matches upstream; the newer (effective) one is stale.
    // An implementation scanning `managed.some(matches)` hands the model back here
    // and leaves it on a stale price.
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [
        row(entry, { id: 'mgd-old', startDate: '2020-01-01T00:00:00Z' }),
        stale(entry, { id: 'mgd-new', startDate: '2026-01-01T00:00:00Z' }),
        stale(entry, { id: 'ovr-1', isLangfuseManaged: false }),
      ],
    })
    expect(actionFor(r.out, JUDGE)).toBe('replace')
    expect(r.mock.creates).toHaveLength(1)
  })

  it('plural overrides — the EFFECTIVE one decides, not "any of them matches"', async () => {
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [
        stale(entry),
        row(entry, { id: 'ovr-old', isLangfuseManaged: false, startDate: '2020-01-01T00:00:00Z' }),
        stale(entry, {
          id: 'ovr-new',
          isLangfuseManaged: false,
          startDate: '2026-01-01T00:00:00Z',
        }),
      ],
    })
    expect(actionFor(r.out, JUDGE)).toBe('replace')
    expect(r.mock.creates).toHaveLength(1)
  })

  it('is idempotent — a run against an instance that already matches writes nothing, twice', async () => {
    const entry = sampleEntry(JUDGE)
    const models = [row(entry)]
    for (let i = 0; i < 2; i++) {
      const r = await run(['--models', JUDGE, '--strict'], { models })
      expect(r.mock.creates).toHaveLength(0)
      expect(r.mock.deletes).toHaveLength(0)
    }
  })

  it('decideAction is pure across all five states', () => {
    const upstream = upstreamOf(JUDGE)
    const entry = sampleEntry(JUDGE)
    const matching = asInstance(row(entry))
    const drifted = asInstance(stale(entry))
    const call = (effManaged?: InstanceModel, effOverride?: InstanceModel) =>
      decideAction({ upstream, effManaged, effOverride, overrideCount: effOverride ? 1 : 0 }).kind
    expect(call(matching, undefined)).toBe('none')
    expect(call(matching, drifted)).toBe('delete')
    expect(call(drifted, undefined)).toBe('create')
    expect(call(undefined, undefined)).toBe('create')
    expect(call(drifted, drifted)).toBe('replace')
    expect(call(drifted, matching)).toBe('none')
  })
})

// ===========================================================================
// Effective-row resolution + ties
// ===========================================================================

describe('effectiveWithin', () => {
  const entry = sampleEntry(JUDGE)
  const at = (id: string, startDate: string | null, mutate = false) =>
    asInstance(mutate ? stale(entry, { id, startDate }) : row(entry, { id, startDate }))

  it('picks the newest startDate and sorts NULL last', () => {
    const rows = [
      at('null', null),
      at('old', '2020-01-01T00:00:00Z'),
      at('new', '2026-01-01T00:00:00Z'),
    ]
    expect(effectiveWithin(rows)?.id).toBe('new')
    expect(effectiveWithin([at('null', null)])?.id).toBe('null')
    // With no dated rows at all, the NULL group is what the server resolves within.
    expect(effectiveWithin([at('n1', null)])?.id).toBe('n1')
    expect(effectiveWithin([])).toBeUndefined()
  })

  it('resolves IDENTICAL tied rows (any server pick is the same definition)', () => {
    const rows = [at('a', null), at('b', null)]
    expect(['a', 'b']).toContain(effectiveWithin(rows)?.id)
  })

  it('REFUSES tied rows that differ — the server has no further tiebreak to mirror', () => {
    const rows = [at('a', null), at('b', null, true)]
    expect(() => effectiveWithin(rows)).toThrow(AmbiguousTieError)
  })

  it('an ambiguous tie is refused with zero writes, and other targets still run', async () => {
    const judgeEntry = sampleEntry(JUDGE)
    const intentEntry = sampleEntry('gpt-5.5-2026-04-23')
    const r = await run(['--models', `${JUDGE},${INTENT}`, '--strict'], {
      models: [
        stale(judgeEntry),
        row(judgeEntry, { id: 'ovr-a', isLangfuseManaged: false }),
        stale(judgeEntry, { id: 'ovr-b', isLangfuseManaged: false }),
        stale(intentEntry),
      ],
    })
    expect(actionFor(r.out, JUDGE)).toBe('refused')
    expect(lineFor(r.out, JUDGE)).toContain('ambiguous: 2 rows tied on startDate')
    expect(r.mock.deletes).toHaveLength(0)
    // The other target is unaffected.
    expect(actionFor(r.out, INTENT)).toBe('create')
    expect(r.mock.creates).toHaveLength(1)
    expect(r.code).not.toBe(0)
  })

  it('selectEffective reports custom over managed, then newest startDate, NULL last', () => {
    const entryRows = [
      asInstance(row(entry, { id: 'mgd' })),
      asInstance(row(entry, { id: 'cust', isLangfuseManaged: false })),
    ]
    expect(selectEffective(entryRows, JUDGE)?.id).toBe('cust')
    const twoCustoms = [
      asInstance(row(entry, { id: 'c-null', isLangfuseManaged: false })),
      asInstance(
        row(entry, { id: 'c-dated', isLangfuseManaged: false, startDate: '2026-01-01T00:00:00Z' })
      ),
    ]
    expect(selectEffective(twoCustoms, JUDGE)?.id).toBe('c-dated')
  })
})

describe('orderedForDelete', () => {
  it('puts the effective row LAST so a mid-sequence failure cannot promote a dormant one', () => {
    const entry = sampleEntry(JUDGE)
    const rows = ['a', 'b', 'c'].map(id => asInstance(row(entry, { id, isLangfuseManaged: false })))
    expect(orderedForDelete(rows, rows[0]).map(r => r.id)).toEqual(['b', 'c', 'a'])
    expect(orderedForDelete(rows, undefined).map(r => r.id)).toEqual(['a', 'b', 'c'])
  })
})

// ===========================================================================
// Partial failure
// ===========================================================================

describe('partial failure', () => {
  const entry = sampleEntry(JUDGE)

  it('POST fails with a managed row present: reported with the delete count, no rollback', async () => {
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [stale(entry), stale(entry, { id: 'ovr-1', isLangfuseManaged: false })],
      failCreate: true,
    })
    expect(actionFor(r.out, JUDGE)).toBe('failed')
    expect(lineFor(r.out, JUDGE)).toContain('deleted 1 override, create failed')
    expect(lineFor(r.out, JUDGE)).not.toContain('UNPRICED')
    expect(r.mock.deletes).toEqual(['ovr-1'])
    // No rollback: nothing is re-created against an API that is already failing.
    expect(r.mock.creates).toHaveLength(0)
    expect(r.code).not.toBe(0)
  })

  it('POST fails on an override-ONLY target: reported as UNPRICED, and a later run re-creates', async () => {
    const models = [stale(entry, { id: 'ovr-1', isLangfuseManaged: false })]
    const first = await run(['--models', JUDGE, '--strict'], { models, failCreate: true })
    expect(actionFor(first.out, JUDGE)).toBe('failed')
    expect(lineFor(first.out, JUDGE)).toContain('NO definition remains, model is now UNPRICED')
    expect(first.code).not.toBe(0)

    // Convergence: the next run observes "no managed row, no override" and creates.
    const second = await run(['--models', JUDGE, '--strict'], { models: [] })
    expect(actionFor(second.out, JUDGE)).toBe('create')
    expect(second.mock.creates).toHaveLength(1)
  })

  it('a DELETE failure stops the replace: no further deletes, no POST, effective row survives', async () => {
    // Three overrides; the second delete fails. Effective-last ordering means the
    // effective row is still among the survivors, so the model resolves exactly as
    // it did before the run.
    const models = [
      stale(entry),
      stale(entry, { id: 'ovr-eff', isLangfuseManaged: false, startDate: '2026-01-01T00:00:00Z' }),
      stale(entry, { id: 'ovr-x', isLangfuseManaged: false }),
      stale(entry, { id: 'ovr-y', isLangfuseManaged: false }),
    ]
    const r = await run(['--models', JUDGE, '--strict'], {
      models,
      failDelete: (_id, nth) => nth === 2,
    })
    expect(actionFor(r.out, JUDGE)).toBe('failed')
    expect(lineFor(r.out, JUDGE)).toContain('deleted 1 of 3 override(s), delete failed')
    expect(r.mock.deletes).toHaveLength(1)
    expect(r.mock.creates).toHaveLength(0)
    // The effective override is never even attempted — it is ordered last.
    expect(r.mock.order).not.toContain('delete ovr-eff')
    expect(r.mock.order).not.toContain('delete-failed ovr-eff')
    expect(r.code).not.toBe(0)
  })

  it('a DELETE failure stops the convergence delete too, with an accurate count', async () => {
    const models = [
      row(entry),
      row(entry, { id: 'ovr-eff', isLangfuseManaged: false, startDate: '2026-01-01T00:00:00Z' }),
      row(entry, { id: 'ovr-x', isLangfuseManaged: false }),
    ]
    const r = await run(['--models', JUDGE, '--strict'], {
      models,
      failDelete: (id: string) => id === 'ovr-x',
    })
    expect(actionFor(r.out, JUDGE)).toBe('failed')
    expect(lineFor(r.out, JUDGE)).toContain('deleted 0 of 2 override(s), delete failed')
    expect(r.mock.deletes).toHaveLength(0)
    expect(r.mock.order).not.toContain('delete ovr-eff')
    expect(r.code).not.toBe(0)
  })

  it('the summary never reports a failure as a success', async () => {
    const r = await run(['--models', JUDGE], {
      models: [stale(entry), stale(entry, { id: 'ovr-1', isLangfuseManaged: false })],
      failCreate: true,
    })
    const summary = r.out.find(l => l.startsWith('summary:')) ?? ''
    expect(summary).toContain('0 created')
    expect(summary).toContain('0 replaced')
    expect(summary).toContain('1 failed')
  })
})

// ===========================================================================
// Selection
// ===========================================================================

describe('selection', () => {
  it('selects by PATTERN, using the verbatim upstream (?i) pattern', () => {
    const models = parseUpstream(UNIQUE_RAW)
    // The intent pass sends `gpt-5.5`; the entry serving it is named differently.
    expect(selectUpstream(models, INTENT).modelName).toBe('gpt-5.5-2026-04-23')
    expect(selectUpstream(models, JUDGE).modelName).toBe(JUDGE)
    expect(selectUpstream(models, 'openai/gpt-5.5').modelName).toBe('gpt-5.5-2026-04-23')
  })

  it('the verbatim pattern is a SyntaxError for a raw new RegExp — the trap this port exists for', () => {
    const pattern = String(sampleEntry(JUDGE).matchPattern)
    expect(pattern.startsWith('(?i)')).toBe(true)
    expect(() => new RegExp(pattern)).toThrow(SyntaxError)
    const compiled = toJsRegex(pattern)
    expect(compiled.flags).toContain('i')
    expect(compiled.test('GPT-5.4-MINI')).toBe(true)
  })

  it('refuses any other inline flag rather than guessing', () => {
    expect(() => toJsRegex('(?m)^x$')).toThrow(PatternError)
    expect(() => toJsRegex('(?s)^x$')).toThrow(PatternError)
    // A pattern that is invalid for a reason OTHER than the inline flag also throws,
    // never selecting nothing and reporting success.
    expect(() => toJsRegex('(?i)^(unclosed')).toThrow(PatternError)
  })

  it('refuses an ambiguous match and writes nothing', async () => {
    const models = parseUpstream(JSON.stringify(AMBIGUOUS_SET))
    expect(() => selectUpstream(models, INTENT)).toThrow(AmbiguousMatchError)
    const r = await run(['--models', INTENT, '--strict'], {
      upstreamText: JSON.stringify(AMBIGUOUS_SET),
      models: [],
    })
    expect(actionFor(r.out, INTENT)).toBe('refused')
    expect(r.mock.creates).toHaveLength(0)
    expect(r.code).not.toBe(0)
  })

  it('reports absence loudly and leaves the existing definition alone', async () => {
    const models = parseUpstream(UNIQUE_RAW)
    expect(() => selectUpstream(models, 'gpt-does-not-exist')).toThrow(NotFoundUpstreamError)
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', 'gpt-does-not-exist', '--strict'], {
      models: [row(entry, { id: 'existing', isLangfuseManaged: false })],
    })
    expect(actionFor(r.out, 'gpt-does-not-exist')).toBe('absent')
    expect(lineFor(r.out, 'gpt-does-not-exist')).toContain('left untouched')
    expect(r.mock.deletes).toHaveLength(0)
    expect(r.mock.creates).toHaveLength(0)
    expect(r.code).not.toBe(0)
  })

  it('splits instance rows into managed and overrides on the reported managed flag', () => {
    const entry = sampleEntry(JUDGE)
    const rows = [
      asInstance(row(entry, { id: 'm' })),
      asInstance(row(entry, { id: 'o', isLangfuseManaged: false })),
      asInstance(row(sampleEntry(LUNA), { id: 'other' })),
    ]
    const split = splitInstanceRows(rows, JUDGE)
    expect(split.managed.map(r => r.id)).toEqual(['m'])
    expect(split.overrides.map(r => r.id)).toEqual(['o'])
  })
})

// ===========================================================================
// Upstream shape validation
// ===========================================================================

describe('parseUpstream', () => {
  const base = (): Record<string, unknown> => clone(sampleEntry(JUDGE))
  const withEntry = (mutate: (e: Record<string, unknown>) => void): string => {
    const e = base()
    mutate(e)
    return JSON.stringify([e])
  }
  const tiers = (e: Record<string, unknown>): Record<string, unknown>[] =>
    e.pricingTiers as Record<string, unknown>[]

  it('accepts the verbatim upstream excerpt, extra server-owned keys and all', () => {
    const models = parseUpstream(UNIQUE_RAW)
    expect(models).toHaveLength(UNIQUE_SET.length)
    expect(models[0].pricingTiers[0]).not.toHaveProperty('id')
  })

  const rejections: Array<[string, string]> = [
    ['non-array root', JSON.stringify({ models: [] })],
    ['null root', 'null'],
    ['empty array root', '[]'],
    ['entry not an object', JSON.stringify(['x'])],
    ['modelName missing', withEntry(e => delete e.modelName)],
    ['modelName non-string', withEntry(e => (e.modelName = 7))],
    ['modelName empty', withEntry(e => (e.modelName = ''))],
    ['matchPattern missing', withEntry(e => delete e.matchPattern)],
    ['matchPattern non-string', withEntry(e => (e.matchPattern = 7))],
    ['matchPattern empty', withEntry(e => (e.matchPattern = ''))],
    ['pricingTiers missing', withEntry(e => delete e.pricingTiers)],
    ['pricingTiers non-array', withEntry(e => (e.pricingTiers = {}))],
    ['pricingTiers empty', withEntry(e => (e.pricingTiers = []))],
    ['isDefault missing', withEntry(e => delete tiers(e)[0].isDefault)],
    ['isDefault numeric 1', withEntry(e => (tiers(e)[0].isDefault = 1))],
    ['isDefault string "true"', withEntry(e => (tiers(e)[0].isDefault = 'true'))],
    ['priority missing', withEntry(e => delete tiers(e)[0].priority)],
    ['priority non-number', withEntry(e => (tiers(e)[0].priority = '0'))],
    ['priority NaN', withEntry(e => (tiers(e)[0].priority = Number.NaN))],
    ['conditions missing', withEntry(e => delete tiers(e)[0].conditions)],
    ['conditions non-array', withEntry(e => (tiers(e)[0].conditions = {}))],
    ['tier name non-string', withEntry(e => (tiers(e)[0].name = 5))],
    ['prices missing', withEntry(e => delete tiers(e)[0].prices)],
    ['prices non-object', withEntry(e => (tiers(e)[0].prices = 1))],
    ['prices array', withEntry(e => (tiers(e)[0].prices = []))],
    ['prices empty map', withEntry(e => (tiers(e)[0].prices = {}))],
    [
      'price non-numeric',
      withEntry(e => ((tiers(e)[0].prices as Record<string, unknown>).input = '1')),
    ],
    [
      'price non-finite',
      withEntry(e => ((tiers(e)[0].prices as Record<string, unknown>).input = 'Infinity')),
    ],
    ['zero default tiers', withEntry(e => (tiers(e)[0].isDefault = false))],
    [
      'two default tiers',
      withEntry(e => {
        const t = tiers(e)
        t.push({ ...clone(t[0]), name: 'Second', priority: 1 })
      }),
    ],
    ['default tier with non-zero priority', withEntry(e => (tiers(e)[0].priority = 1))],
    [
      'default tier with conditions',
      withEntry(
        e => (tiers(e)[0].conditions = [{ usageDetailPattern: 'input', operator: 'gt', value: 1 }])
      ),
    ],
    ['tokenizerId non-string', withEntry(e => (e.tokenizerId = 3))],
    ['an HTML error page', '<!DOCTYPE html><html><body>404</body></html>'],
    ['a truncated JSON body', UNIQUE_RAW.slice(0, 400)],
  ]

  for (const [label, payload] of rejections) {
    it(`rejects ${label}`, () => {
      expect(() => parseUpstream(payload)).toThrow(UpstreamShapeError)
    })
  }

  it('an empty price map is rejected — otherwise it writes a definition pricing everything at zero', () => {
    const payload = withEntry(e => (tiers(e)[0].prices = {}))
    expect(() => parseUpstream(payload)).toThrow(/prices is empty/)
  })

  it('a rejected payload writes NOTHING at all — not even for the targets that parsed', async () => {
    const entry = sampleEntry(JUDGE)
    const r = await run(['--models', `${JUDGE},${INTENT}`, '--strict'], {
      upstreamText: '<!DOCTYPE html><html>nope</html>',
      models: [stale(entry)],
    })
    expect(r.mock.creates).toHaveLength(0)
    expect(r.mock.deletes).toHaveLength(0)
    expect(r.err.join('\n')).toContain('upstream payload rejected')
    expect(r.code).not.toBe(0)
  })
})

describe('parseInstanceRow', () => {
  const entry = sampleEntry(JUDGE)
  it('fails CLOSED on a row whose managed flag is missing or not a boolean', () => {
    expect(() => asInstance(row(entry, { isLangfuseManaged: undefined }))).toThrow(
      InstanceShapeError
    )
    expect(() => asInstance(row(entry, { isLangfuseManaged: 'false' }))).toThrow(InstanceShapeError)
  })
  it('fails CLOSED on an unparseable startDate', () => {
    expect(() => asInstance(row(entry, { startDate: 'whenever' }))).toThrow(InstanceShapeError)
  })
  it('accepts a legacy flat-priced row with no tiers (a real state, not a malformed one)', () => {
    const legacy = asInstance(row(entry, { pricingTiers: [] }))
    expect(legacy.pricingTiers).toEqual([])
    // It simply differs from every tiered upstream entry.
    expect(diffPrices(legacy, upstreamOf(JUDGE)).length).toBeGreaterThan(0)
  })
})

// ===========================================================================
// Diff + guards
// ===========================================================================

describe('diff', () => {
  const entry = sampleEntry(JUDGE)
  const upstream = upstreamOf(JUDGE)

  it('ignores `unit` and the deprecated top-level prices', () => {
    // The fixture row already carries unit: null and a flat `prices` view that
    // disagrees with its tiers.
    expect(diffPrices(asInstance(row(entry)), upstream)).toEqual([])
  })

  it('is insensitive to key ORDER — otherwise the override is replaced every night', () => {
    const reordered = clone(entry.pricingTiers) as Array<Record<string, unknown>>
    const prices = reordered[0].prices as Record<string, number>
    reordered[0].prices = Object.fromEntries(Object.entries(prices).reverse())
    // Re-key the tier object itself in a different order too.
    reordered[0] = Object.fromEntries(Object.entries(reordered[0]).reverse())
    expect(diffPrices(asInstance(row(entry, { pricingTiers: reordered })), upstream)).toEqual([])
  })

  it('treats conditions ORDER as significant — an ordered predicate list is not a set', () => {
    const luna = sampleEntry(LUNA)
    const lunaUpstream = upstreamOf(LUNA)
    const tiers = clone(luna.pricingTiers) as Array<{ conditions: unknown[] }>
    tiers[1].conditions = [
      { usageDetailPattern: 'output', operator: 'gt', value: 1, caseSensitive: false },
      ...tiers[1].conditions,
    ]
    expect(
      diffPrices(asInstance(row(luna, { pricingTiers: tiers })), lunaUpstream).length
    ).toBeGreaterThan(0)
  })

  it('reports a matchPattern-only change as drift', async () => {
    const drifted = row(entry, { matchPattern: '(?i)^(gpt-5\\.4-mini)$' })
    // A stale pattern means the judge's model string may stop resolving at all.
    expect(
      diffPrices(asInstance(drifted), upstream).some(d => d.detail.startsWith('matchPattern'))
    ).toBe(true)
    const r = await run(['--models', JUDGE, '--strict'], { models: [drifted] })
    expect(actionFor(r.out, JUDGE)).toBe('create')
  })

  it('reports a removed tier as a per-usage-type absence, not only a structural note', () => {
    const luna = sampleEntry(LUNA)
    const single = (clone(luna.pricingTiers) as unknown[]).slice(0, 1)
    const deltas = diffPrices(upstreamOf(LUNA), asInstance(row(luna, { pricingTiers: single })))
    expect(deltas.some(d => d.detail === 'tier removed')).toBe(true)
    expect(deltas.some(d => d.usageType !== undefined && d.to === undefined)).toBe(true)
  })
})

describe('guardDelta', () => {
  const upstream = upstreamOf(JUDGE)
  const entry = sampleEntry(JUDGE)
  const scaled = (factor: number) => {
    const tiers = clone(entry.pricingTiers) as Array<{ prices: Record<string, number> }>
    tiers[0].prices.input = tiers[0].prices.input / factor
    return row(entry, { pricingTiers: tiers })
  }

  it('refuses a 10x increase and allows it under --force', async () => {
    const models = [scaled(10)]
    const refused = await run(['--models', JUDGE, '--strict'], { models })
    expect(actionFor(refused.out, JUDGE)).toBe('refused')
    expect(lineFor(refused.out, JUDGE)).toContain('implausible delta: input')
    expect(lineFor(refused.out, JUDGE)).toContain('(10.0x)')
    expect(refused.mock.creates).toHaveLength(0)
    expect(refused.code).not.toBe(0)

    const forced = await run(['--models', JUDGE, '--strict', '--force'], { models })
    expect(actionFor(forced.out, JUDGE)).toBe('create')
    expect(forced.mock.creates).toHaveLength(1)
  })

  it('refuses a 10x DECREASE the same way (the guard is symmetric)', async () => {
    const r = await run(['--models', JUDGE, '--strict'], { models: [scaled(0.1)] })
    expect(actionFor(r.out, JUDGE)).toBe('refused')
    expect(lineFor(r.out, JUDGE)).toContain('(10.0x)')
    expect(r.mock.creates).toHaveLength(0)
  })

  it('allows EXACTLY 5x in both directions without --force (exclusive boundary)', async () => {
    for (const factor of [5, 0.2]) {
      const r = await run(['--models', JUDGE, '--strict'], { models: [scaled(factor)] })
      expect(actionFor(r.out, JUDGE)).toBe('create')
      expect(r.mock.creates).toHaveLength(1)
      expect(r.code).toBe(0)
    }
  })

  it('refuses degenerate values — zero, negative, and a usage type that vanished', () => {
    const from = asInstance(row(entry))
    const zeroed = clone(upstream) as UpstreamModel
    zeroed.pricingTiers[0].prices.input = 0
    expect(guardDelta(diffPrices(from, zeroed)).ok).toBe(false)

    const negative = clone(upstream) as UpstreamModel
    negative.pricingTiers[0].prices.input = -1e-6
    expect(guardDelta(diffPrices(from, negative)).ok).toBe(false)

    const missing = clone(upstream) as UpstreamModel
    delete missing.pricingTiers[0].prices.input
    const verdict = guardDelta(diffPrices(from, missing))
    expect(verdict.ok).toBe(false)
    expect(verdict.reason).toContain('absent upstream')
  })

  it('allows a NEWLY priced usage type (nothing to be implausible against)', () => {
    const from = asInstance(row(entry))
    const added = clone(upstream) as UpstreamModel
    added.pricingTiers[0].prices.brand_new = 1e-3
    expect(guardDelta(diffPrices(from, added)).ok).toBe(true)
  })

  it('isolates a refusal: another target in the same run still writes', async () => {
    const intentEntry = sampleEntry('gpt-5.5-2026-04-23')
    const r = await run(['--models', `${JUDGE},${INTENT}`], {
      models: [scaled(10), stale(intentEntry)],
    })
    expect(actionFor(r.out, JUDGE)).toBe('refused')
    expect(actionFor(r.out, INTENT)).toBe('create')
    expect(r.mock.creates).toHaveLength(1)
  })
})

// ===========================================================================
// The hand-back is a price change too
// ===========================================================================

describe('convergence guard', () => {
  const entry = sampleEntry(JUDGE)
  // Managed matches upstream (so the action is a hand-back) while the override in
  // force prices this model 10x lower. Deleting it APPLIES that 10x move.
  const handBackBy = (factor: number) => {
    const tiers = clone(entry.pricingTiers) as Array<{ prices: Record<string, number> }>
    tiers[0].prices.input = tiers[0].prices.input / factor
    return [row(entry), row(entry, { id: 'ovr-1', isLangfuseManaged: false, pricingTiers: tiers })]
  }

  it('REFUSES a hand-back whose price movement is implausible, and writes nothing', async () => {
    const r = await run(['--models', JUDGE, '--strict'], { models: handBackBy(10) })
    expect(actionFor(r.out, JUDGE)).toBe('refused')
    expect(lineFor(r.out, JUDGE)).toContain('implausible delta: input')
    expect(lineFor(r.out, JUDGE)).toContain('(10.0x)')
    expect(r.mock.deletes).toHaveLength(0)
    expect(r.mock.creates).toHaveLength(0)
    expect(r.code).not.toBe(0)
  })

  it('proceeds with the hand-back under --force', async () => {
    const r = await run(['--models', JUDGE, '--strict', '--force'], { models: handBackBy(10) })
    expect(actionFor(r.out, JUDGE)).toBe('delete')
    expect(r.mock.deletes).toEqual(['ovr-1'])
    expect(r.code).toBe(0)
  })

  it('still hands back when the movement is within the guard', async () => {
    const r = await run(['--models', JUDGE, '--strict'], { models: handBackBy(2) })
    expect(actionFor(r.out, JUDGE)).toBe('delete')
    expect(r.mock.deletes).toEqual(['ovr-1'])
  })

  it('guardDeltasFor measures the change that TAKES EFFECT on every mutating action', () => {
    const upstream = upstreamOf(JUDGE)
    const matching = asInstance(row(entry))
    const drifted = asInstance(stale(entry))
    const kinds = {
      delete: { kind: 'delete' as const, reason: '' },
      create: { kind: 'create' as const, reason: '' },
      replace: { kind: 'replace' as const, reason: '' },
      none: { kind: 'none' as const, reason: '' },
    }
    // Hand-back: override -> managed, NOT managed -> upstream (which is empty by
    // definition here, so a guard reading it could never fire).
    const handBack = guardDeltasFor({
      action: kinds.delete,
      upstream,
      effManaged: matching,
      effOverride: drifted,
    })
    expect(handBack.length).toBeGreaterThan(0)
    expect(diffPrices(matching, upstream)).toEqual([])
    // Create/replace: the definition in force -> upstream.
    expect(
      guardDeltasFor({
        action: kinds.replace,
        upstream,
        effManaged: matching,
        effOverride: drifted,
      }).length
    ).toBeGreaterThan(0)
    expect(
      guardDeltasFor({
        action: kinds.create,
        upstream,
        effManaged: drifted,
        effOverride: undefined,
      }).length
    ).toBeGreaterThan(0)
    // Nothing takes effect, nothing to guard.
    expect(
      guardDeltasFor({ action: kinds.none, upstream, effManaged: drifted, effOverride: drifted })
    ).toEqual([])
  })
})

// ===========================================================================
// Uniqueness is on (project, modelName) — the pattern is not part of the key
// ===========================================================================

describe('name-blocking overrides', () => {
  const entry = sampleEntry(JUDGE)
  // Upstream widened this model's regex; our override still carries the OLD pattern,
  // which no longer matches the target — but it keeps the same modelName, so it
  // blocks every future create.
  const stalePattern = (over: Record<string, unknown> = {}) =>
    row(entry, {
      id: 'ovr-old-pattern',
      isLangfuseManaged: false,
      matchPattern: '(?i)^(gpt-5\\.4-mini-legacy)$',
      ...over,
    })

  it('finds them by NAME, so a changed upstream pattern is not a permanent deadlock', async () => {
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [stale(entry), stalePattern()],
    })
    // The transport enforces the real uniqueness, so a pattern-only search would
    // land a 400 here and the model could never converge.
    expect(actionFor(r.out, JUDGE)).toBe('create')
    expect(r.mock.order).toEqual(['delete ovr-old-pattern', `create ${String(entry.modelName)}`])
    expect(r.out.some(l => l.includes('with a non-matching pattern'))).toBe(true)
    expect(r.code).toBe(0)
  })

  it('removes them alongside the matching overrides on a replace, effective LAST', async () => {
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [
        stale(entry),
        stale(entry, {
          id: 'ovr-eff',
          isLangfuseManaged: false,
          startDate: '2026-01-01T00:00:00Z',
        }),
        stalePattern(),
      ],
    })
    expect(actionFor(r.out, JUDGE)).toBe('replace')
    expect(r.mock.order).toEqual([
      'delete ovr-old-pattern',
      'delete ovr-eff',
      `create ${String(entry.modelName)}`,
    ])
  })

  it('removes them on a hand-back too, so the model is fully handed back', async () => {
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [row(entry), row(entry, { id: 'ovr-1', isLangfuseManaged: false }), stalePattern()],
    })
    expect(actionFor(r.out, JUDGE)).toBe('delete')
    expect(r.mock.deletes.sort()).toEqual(['ovr-1', 'ovr-old-pattern'])
  })

  it('never counts a managed row or a pattern-matching row as blocking', () => {
    const rows = [
      asInstance(row(entry, { id: 'managed-same-name' })),
      asInstance(row(entry, { id: 'matching-override', isLangfuseManaged: false })),
      asInstance(stalePattern()),
      asInstance(row(sampleEntry('text-bison'), { id: 'other-name', isLangfuseManaged: false })),
    ]
    expect(blockingOverrides(rows, JUDGE, String(entry.modelName)).map(r => r.id)).toEqual([
      'ovr-old-pattern',
    ])
  })

  it('--reset also clears a row whose pattern no longer matches its own name', async () => {
    const r = await run(['--reset', JUDGE], { models: [stalePattern()] })
    expect(r.mock.deletes).toEqual(['ovr-old-pattern'])
    expect(r.code).toBe(0)
  })
})

// ===========================================================================
// Distinct target strings can select ONE upstream definition
// ===========================================================================

describe('target aliases', () => {
  const DATED = 'gpt-5.5-2026-04-23'

  it('writes ONCE for two aliases — the second sees the first write and has nothing to do', async () => {
    // gpt-5.5's live pattern matches both the bare and the dated string.
    const r = await run(['--models', `${INTENT},${DATED}`, '--strict'], { models: [] })
    expect(r.mock.creates).toHaveLength(1)
    expect(actionFor(r.out, INTENT)).toBe('create')
    // Not skipped — inspected against re-read rows and found already correct.
    expect(actionFor(r.out, DATED)).toBe('none')
    expect(lineFor(r.out, DATED)).toContain('override already matches upstream')
    const summary = r.out.find(l => l.startsWith('summary:')) ?? ''
    expect(summary).toContain('2 targets')
    expect(summary).toContain('1 created')
    expect(summary).toContain('0 failed')
    expect(r.code).toBe(0)
  })

  it('reconciles a stale override that matches ONLY the second alias', async () => {
    // The regression a skip-the-target dedup causes: the first alias sees a current
    // managed row and no matching override, reports none, and the second alias never
    // gets looked at — leaving its stale override in force while the run reads clean.
    const entry = sampleEntry(DATED)
    const tiers = clone(entry.pricingTiers) as Array<{ prices: Record<string, number> }>
    tiers[0].prices.input *= 2
    const r = await run(['--models', `${INTENT},${DATED}`, '--strict'], {
      models: [
        row(entry),
        row(entry, {
          id: 'ovr-dated-only',
          isLangfuseManaged: false,
          // Matches the dated alias only.
          matchPattern: '(?i)^(gpt-5\\.5-2026-04-23)$',
          pricingTiers: tiers,
        }),
      ],
    })
    expect(actionFor(r.out, INTENT)).toBe('none')
    expect(actionFor(r.out, DATED)).toBe('delete')
    expect(r.mock.deletes).toEqual(['ovr-dated-only'])
    expect(r.code).toBe(0)
  })

  it('does not conflate targets that select DIFFERENT definitions', async () => {
    const r = await run(['--models', `${JUDGE},${INTENT}`, '--strict'], { models: [] })
    expect(r.mock.creates).toHaveLength(2)
  })

  it('refuses to judge a later target against rows it could not re-read', async () => {
    const r = await run(['--models', `${INTENT},${JUDGE}`, '--strict'], {
      models: [],
      failListCall: 2,
    })
    expect(actionFor(r.out, INTENT)).toBe('create')
    expect(actionFor(r.out, JUDGE)).toBe('failed')
    expect(lineFor(r.out, JUDGE)).toContain('could not re-read the instance')
    // Only the first target's write happened; the second never acted on stale rows.
    expect(r.mock.creates).toHaveLength(1)
    expect(r.code).not.toBe(0)
  })

  it('does not re-read when nothing was written (the normal case)', async () => {
    const r = await run(['--models', `${JUDGE},${INTENT}`, '--strict'], {
      models: [row(sampleEntry(JUDGE)), row(sampleEntry(DATED))],
      failListCall: 2,
    })
    // A second list call would have failed; zero writes means none was needed.
    expect(actionFor(r.out, JUDGE)).toBe('none')
    expect(actionFor(r.out, INTENT)).toBe('none')
    expect(r.code).toBe(0)
  })
})

// ===========================================================================
// --dry-run is TOTAL: every mutating path previews, none writes
// ===========================================================================

describe('dry-run', () => {
  const entry = sampleEntry(JUDGE)
  const handBack = [
    row(entry),
    row(entry, {
      id: 'ovr-1',
      isLangfuseManaged: false,
      pricingTiers: (() => {
        const t = clone(entry.pricingTiers) as Array<{ prices: Record<string, number> }>
        t[0].prices.input *= 2
        return t
      })(),
    }),
  ]

  const cases: Array<[string, string, Record<string, unknown>[]]> = [
    ['create', 'create', [stale(entry)]],
    ['replace', 'replace', [stale(entry), stale(entry, { id: 'ovr-1', isLangfuseManaged: false })]],
    ['hand-back delete', 'delete', handBack],
  ]
  for (const [label, want, models] of cases) {
    it(`previews the ${label} and issues zero writes`, async () => {
      const r = await run(['--models', JUDGE, '--dry-run', '--strict'], { models })
      expect(actionFor(r.out, JUDGE)).toBe(want)
      expect(lineFor(r.out, JUDGE)).toContain('dry-run, nothing written')
      expect(r.mock.creates).toHaveLength(0)
      expect(r.mock.deletes).toHaveLength(0)
    })
  }

  it('previews --reset instead of deleting — the flag pairing that exists BECAUSE reset is destructive', async () => {
    const r = await run(['--reset', JUDGE, '--dry-run'], {
      models: [
        row(entry, { id: 'managed-1' }),
        row(entry, { id: 'ovr-1', isLangfuseManaged: false }),
        row(entry, { id: 'ovr-2', isLangfuseManaged: false }),
      ],
    })
    expect(r.mock.deletes).toHaveLength(0)
    // It still SHOWS what it would do — a preview that lists nothing is not a preview.
    expect(r.out.some(l => l.includes('would delete ovr-1'))).toBe(true)
    expect(r.out.some(l => l.includes('would delete ovr-2'))).toBe(true)
    expect(
      r.out.some(l => l.includes('would delete 2 project overrides (dry-run, nothing written)'))
    ).toBe(true)
    // A managed row is never in the preview either.
    expect(r.out.some(l => l.includes('managed-1'))).toBe(false)
    expect(r.code).toBe(0)
  })

  it('--reset with absent credentials reports FAILURE, never a silent clean-up', async () => {
    const r = await run(['--reset', JUDGE], { models: [] }, { LANGFUSE_HOST: 'http://lf' })
    expect(r.code).not.toBe(0)
    expect(r.err.join('\n')).toContain('nothing synced')
    expect(r.mock.deletes).toHaveLength(0)
  })
})

// ===========================================================================
// Create body fidelity
// ===========================================================================

describe('buildCreateBody', () => {
  it('round-trips a real TWO-tier entry without flattening', async () => {
    const luna = sampleEntry(LUNA)
    const r = await run(['--models', LUNA, '--strict'], { models: [stale(luna)] })
    expect(r.mock.creates).toHaveLength(1)
    const body = r.mock.creates[0]
    const tiers = body.pricingTiers as Array<Record<string, unknown>>
    const upstreamTiers = luna.pricingTiers as Array<Record<string, unknown>>
    expect(tiers).toHaveLength(2)
    expect(tiers[1].conditions).toEqual(upstreamTiers[1].conditions)
    expect(tiers[1].priority).toBe(upstreamTiers[1].priority)
    expect(tiers[1].isDefault).toBe(false)
    expect(tiers[1].name).toBe(upstreamTiers[1].name)
    expect(tiers[1].prices).toEqual(upstreamTiers[1].prices)
    expect(body).not.toHaveProperty('inputPrice')
    expect(body).not.toHaveProperty('outputPrice')
    expect(body).not.toHaveProperty('totalPrice')
    expect(body.unit).toBe('TOKENS')
  })

  it('writes the matchPattern BYTE-for-byte, never reconstructed from the model name', async () => {
    const entry = sampleEntry('gpt-5.5-2026-04-23')
    const r = await run(['--models', INTENT, '--strict'], { models: [stale(entry)] })
    expect(r.mock.creates[0].matchPattern).toBe(entry.matchPattern)
    // A reconstruction from the model name would be this, and would not serve the
    // model string the intent pass actually sends.
    expect(r.mock.creates[0].matchPattern).not.toBe('(?i)^(gpt-5.5-2026-04-23)$')
    expect(r.mock.creates[0].modelName).toBe('gpt-5.5-2026-04-23')
  })

  it('round-trips tokenizer fields verbatim, and omits both keys when upstream has neither', () => {
    const withTokenizer = buildCreateBody(upstreamOf(JUDGE))
    const entry = sampleEntry(JUDGE)
    expect(withTokenizer.tokenizerId).toBe(entry.tokenizerId)
    expect(withTokenizer.tokenizerConfig).toEqual(entry.tokenizerConfig)

    const without = buildCreateBody(upstreamOf('text-bison'))
    expect(Object.keys(without)).not.toContain('tokenizerId')
    expect(Object.keys(without)).not.toContain('tokenizerConfig')
  })

  it('drops upstream’s server-owned tier ids (they identify another deployment’s rows)', () => {
    const body = buildCreateBody(upstreamOf(LUNA))
    for (const tier of body.pricingTiers as UpstreamTier[]) {
      expect(tier).not.toHaveProperty('id')
    }
  })
})

// ===========================================================================
// Transport
// ===========================================================================

describe('upstream fetch', () => {
  it('sends NO Authorization header to the public host (and the recorder CAN see one)', async () => {
    const r = await run(['--models', JUDGE, '--strict'], { models: [] })
    expect(r.mock.upstreamRequests).toHaveLength(1)
    const authKeys = r.mock.upstreamRequests[0].headerKeys.filter(
      k => k.toLowerCase() === 'authorization'
    )
    expect(authKeys).toEqual([])
    // Falsifiability: the SAME recorder sees the Langfuse calls carrying one, so an
    // empty result above is evidence rather than an artifact of not looking.
    expect(
      r.mock.langfuseRequests.some(c => c.headerKeys.some(k => k.toLowerCase() === 'authorization'))
    ).toBe(true)
  })

  it('hashes the EXACT bytes fetched, not a re-serialization', async () => {
    // Whitespace and key order in the fixture differ from what JSON.stringify of the
    // parsed object would produce, so a re-serialized hash would differ.
    const expected = crypto.createHash('sha256').update(UNIQUE_RAW).digest('hex')
    expect(crypto.createHash('sha256').update(JSON.stringify(UNIQUE_SET)).digest('hex')).not.toBe(
      expected
    )
    const r = await run(['--models', JUDGE, '--strict'], { models: [] })
    expect(r.out).toContain(`upstream_sha256=${expected}`)
  })

  it('treats a non-2xx as a failure, never an empty payload', async () => {
    const r = await run(['--models', JUDGE, '--strict'], { upstreamStatus: 404, models: [] })
    expect(r.err.join('\n')).toContain('upstream unavailable')
    expect(r.mock.creates).toHaveLength(0)
    expect(r.code).not.toBe(0)
  })

  it('is bounded — a never-settling fetch is reported, not waited on', async () => {
    const started = Date.now()
    const r = await run(['--models', JUDGE, '--strict'], { upstreamNeverSettles: true, models: [] })
    expect(Date.now() - started).toBeLessThan(5_000)
    expect(r.err.join('\n')).toContain('AbortError')
    expect(r.code).not.toBe(0)
  })

  it('fetchUpstream rejects on a timeout rather than hanging', async () => {
    const stalling = (async (_u: string, init?: { signal?: AbortSignal }) =>
      new Promise<Response>((_res, rej) => {
        init?.signal?.addEventListener('abort', () => {
          const e = new Error('The operation was aborted')
          e.name = 'AbortError'
          rej(e)
        })
      })) as unknown as typeof fetch
    await expect(fetchUpstream('https://example.invalid/x', 10, stalling)).rejects.toThrow(
      /AbortError/
    )
  })
})

describe('instance read', () => {
  it('walks EVERY page — a target present only on a later page is found', async () => {
    const entry = sampleEntry(JUDGE)
    const filler = Array.from({ length: 4 }, (_, i) =>
      row(sampleEntry('text-bison'), { id: `filler-${i}` })
    )
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [...filler, row(entry)],
      perPage: 2,
    })
    // Found on page 3 -> no drift. A single-page read would report a create.
    expect(actionFor(r.out, JUDGE)).toBe('none')
    expect(r.mock.creates).toHaveLength(0)
  })

  it('bounds EVERY page request — a never-settling PAGE 2 fails instead of hanging', async () => {
    const filler = Array.from({ length: 4 }, (_, i) =>
      row(sampleEntry('text-bison'), { id: `filler-${i}` })
    )
    const started = Date.now()
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [...filler, row(sampleEntry(JUDGE))],
      perPage: 2,
      pageNeverSettles: 2,
    })
    expect(Date.now() - started).toBeLessThan(5_000)
    expect(r.err.join('\n')).toContain('could not read')
    expect(r.err.join('\n')).toContain('AbortError')
    expect(r.mock.creates).toHaveLength(0)
    expect(r.code).not.toBe(0)
  })

  it('fails CLOSED on a malformed row rather than reading it as absent', async () => {
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [{ id: 'x', modelName: 'y', matchPattern: '(?i)^(y)$' }],
    })
    expect(r.err.join('\n')).toContain('InstanceShapeError')
    expect(r.mock.creates).toHaveLength(0)
    expect(r.code).not.toBe(0)
  })
})

// ===========================================================================
// CLI
// ===========================================================================

describe('CLI', () => {
  const entry = sampleEntry(JUDGE)

  it('fails OPEN by default and non-zero under --strict, writing nothing either way', async () => {
    const open = await run(['--models', JUDGE], { upstreamStatus: 500, models: [] })
    expect(open.code).toBe(0)
    expect(open.mock.creates).toHaveLength(0)
    const strict = await run(['--models', JUDGE, '--strict'], { upstreamStatus: 500, models: [] })
    expect(strict.code).not.toBe(0)
    expect(strict.mock.creates).toHaveLength(0)
  })

  it('touches ONLY the targets — never the rest of the upstream table', async () => {
    const r = await run(['--models', INTENT, '--strict'], { models: [] })
    expect(r.mock.creates.map(c => c.modelName)).toEqual(['gpt-5.5-2026-04-23'])
  })

  it('takes its targets from the round’s active models when --models is absent', async () => {
    const r = await run(['--strict'], { models: [] }, { ...ENV, QA_INTENT_MODEL: 'gpt-5.6-terra' })
    expect(actionFor(r.out, 'gpt-5.6-terra')).toBe('create')
    expect(actionFor(r.out, JUDGE)).toBe('create')
  })

  it('surfaces an EMPTY model override loudly instead of pricing a model nothing sends', async () => {
    const r = await run(['--strict'], { models: [] }, { ...ENV, QA_JUDGE_MODEL: '' })
    expect(actionFor(r.out, '')).toBe('absent')
    expect(r.code).not.toBe(0)
  })

  it('--dry-run prints the intended action and issues zero writes', async () => {
    const r = await run(['--models', JUDGE, '--dry-run', '--strict'], {
      models: [stale(entry), stale(entry, { id: 'ovr-1', isLangfuseManaged: false })],
    })
    expect(actionFor(r.out, JUDGE)).toBe('replace')
    expect(lineFor(r.out, JUDGE)).toContain('dry-run, nothing written')
    expect(r.mock.creates).toHaveLength(0)
    expect(r.mock.deletes).toHaveLength(0)
  })

  it('--reset deletes only project-scoped rows, never a managed one', async () => {
    const r = await run(['--reset', JUDGE], {
      models: [
        row(entry, { id: 'managed-1' }),
        row(entry, { id: 'ovr-1', isLangfuseManaged: false }),
        row(entry, { id: 'ovr-2', isLangfuseManaged: false }),
      ],
    })
    expect(r.mock.deletes.sort()).toEqual(['ovr-1', 'ovr-2'])
    expect(r.mock.creates).toHaveLength(0)
    expect(r.out.some(l => l === `reset ${JUDGE}: deleted 2 project overrides`)).toBe(true)
    expect(r.code).toBe(0)
  })

  it('--reset reports a delete failure non-zero even without --strict', async () => {
    const r = await run(['--reset', JUDGE], {
      models: [row(entry, { id: 'ovr-1', isLangfuseManaged: false })],
      failDelete: () => true,
    })
    expect(r.code).not.toBe(0)
    expect(r.err.join('\n')).toContain('delete failed')
  })

  it('--upstream reads a local payload and still reports its sha256', async () => {
    const local = JSON.stringify([sampleEntry(JUDGE)])
    const mock = mockTransport({ models: [] })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const out: string[] = []
    try {
      const code = await main(['--models', JUDGE, '--upstream', '/tmp/x.json', '--strict'], ENV, {
        fetchImpl: mock.fetchImpl,
        log: s => out.push(s),
        errlog: () => {},
        readFile: () => local,
        timeouts: FAST,
      })
      expect(code).toBe(0)
    } finally {
      vi.unstubAllGlobals()
    }
    // No upstream request at all, and the sha covers the file's bytes.
    expect(mock.upstreamRequests).toHaveLength(0)
    expect(out).toContain(
      `upstream_sha256=${crypto.createHash('sha256').update(local).digest('hex')}`
    )
  })

  it('rejects a misinvocation with the usage code, never fail-open', async () => {
    const r = await run(['--models'], { models: [] })
    expect(r.code).toBe(2)
    expect(r.mock.upstreamRequests).toHaveLength(0)
  })

  it('warns on a version the resolution logic was not verified against, and does NOT block', async () => {
    const r = await run(['--models', JUDGE, '--strict'], {
      models: [row(entry)],
      health: { status: 'OK', version: '9.9.9' },
    })
    expect(r.err.join('\n')).toContain('differs from the verified')
    expect(r.code).toBe(0)
    expect(actionFor(r.out, JUDGE)).toBe('none')
  })

  it('a health body with no version, and a failing health read, both warn without blocking', async () => {
    const bare = await run(['--models', JUDGE, '--strict'], { models: [row(entry)], health: {} })
    expect(bare.err.join('\n')).toContain('no version')
    expect(bare.code).toBe(0)
    const broken = await run(['--models', JUDGE, '--strict'], {
      models: [row(entry)],
      health: 'error',
    })
    expect(broken.err.join('\n')).toContain('skew unchecked')
    expect(broken.code).toBe(0)
  })

  it('reports missing credentials rather than silently syncing nothing', async () => {
    const r = await run(
      ['--models', JUDGE, '--strict'],
      { models: [] },
      { LANGFUSE_HOST: 'http://lf' }
    )
    expect(r.err.join('\n')).toContain('nothing synced')
    expect(r.code).not.toBe(0)
  })

  it('prints the machine-readable summary with every outcome counted', async () => {
    const r = await run(['--models', `${JUDGE},${INTENT}`, '--strict'], { models: [row(entry)] })
    const summary = r.out.find(l => l.startsWith('summary:')) ?? ''
    expect(summary).toMatch(
      /^summary: 2 targets, \d+ created, \d+ replaced, \d+ deleted, \d+ refused, \d+ absent, \d+ failed$/
    )
  })
})

describe('fixture integrity', () => {
  it('the decoy really does double-match the intent target (the ambiguity is staged, not assumed)', () => {
    expect(toJsRegex(String(DECOY.matchPattern)).test(INTENT)).toBe(true)
    expect(toJsRegex(String(sampleEntry('gpt-5.5-2026-04-23').matchPattern)).test(INTENT)).toBe(
      true
    )
  })
})
