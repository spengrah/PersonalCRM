import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiGetAllPages, PaginationError, type LangfuseConfig } from './langfuse'
import { main } from './setup'
import {
  SCORE_CONFIGS,
  TRIAGE_QUEUE_NAME,
  TRIAGE_QUEUE_SCORE_CONFIGS,
  VERDICT_SCORE_NAME,
} from './triage-config'

const cfg: LangfuseConfig = { host: 'http://lf', publicKey: 'p', secretKey: 's' }
const creds = {
  LANGFUSE_HOST: cfg.host,
  LANGFUSE_PUBLIC_KEY: cfg.publicKey,
  LANGFUSE_SECRET_KEY: cfg.secretKey,
}

// ---------- shared fetch-mock plumbing ----------

interface Recorded {
  url: string
  method: string
  body?: Record<string, unknown>
}

// A response scripted to be returned in call order; running past the end THROWS so a
// runaway pagination loop surfaces as a test failure instead of hanging.
function scripted(responses: Array<{ status?: number; json: unknown }>): {
  impl: typeof fetch
  calls: Recorded[]
} {
  const calls: Recorded[] = []
  let i = 0
  const impl = (async (url: string | URL, init?: { method?: string; body?: unknown }) => {
    calls.push({
      url: String(url),
      method: init?.method ?? 'GET',
      body: init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined,
    })
    if (i >= responses.length) throw new Error(`unexpected extra fetch: ${String(url)}`)
    const r = responses[i++]
    const status = r.status ?? 200
    return {
      ok: status >= 200 && status < 300,
      status,
      text: async () => JSON.stringify(r.json),
    } as Response
  }) as unknown as typeof fetch
  return { impl, calls }
}

const pageResp = (data: unknown[], page: number, totalPages: number): { json: unknown } => ({
  json: { data, meta: { page, limit: 100, totalItems: data.length, totalPages } },
})

const cursorResp = (data: unknown[], cursor?: string | null): { json: unknown } => ({
  json: { data, meta: cursor === undefined ? { limit: 100 } : { limit: 100, cursor } },
})

// ---------- tenant mock for setup.main ----------

interface ScoreConfigObj {
  id: string
  name: string
  dataType: string
  isArchived: boolean
  categories: Array<{ label: string; value: number }>
}
interface QueueObj {
  id: string
  name: string
  scoreConfigIds: string[]
}

function goodConfig(name: string, id = `${name}-id`): ScoreConfigObj {
  const spec = SCORE_CONFIGS.find(s => s.name === name)
  if (!spec) throw new Error(`no spec for ${name}`)
  return {
    id,
    name,
    dataType: 'CATEGORICAL',
    isArchived: false,
    categories: spec.categories.map(c => ({ ...c })),
  }
}

// A stateful tenant: paginated GETs over the supplied pages, POST-create echoes a
// fresh object. `failCreate` makes the first matching POST return 500.
function tenantMock(state: {
  configPages?: ScoreConfigObj[][]
  queuePages?: QueueObj[][]
  failCreate?: 'config' | 'queue'
}): { impl: typeof fetch; calls: Recorded[] } {
  const configPages = state.configPages ?? [[]]
  const queuePages = state.queuePages ?? [[]]
  const calls: Recorded[] = []
  let seq = 0
  const okJson = (obj: unknown, status = 200): Response =>
    ({ ok: status < 400, status, text: async () => JSON.stringify(obj) }) as Response
  const pageFrom = (pages: unknown[][], url: string): Response => {
    const p = Number(new URL(url).searchParams.get('page'))
    const data = pages[p - 1] ?? []
    return okJson({
      data,
      meta: { page: p, limit: 100, totalItems: pages.flat().length, totalPages: pages.length },
    })
  }
  const impl = (async (url: string | URL, init?: { method?: string; body?: unknown }) => {
    const u = String(url)
    const method = init?.method ?? 'GET'
    const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined
    calls.push({ url: u, method, body })
    if (method === 'GET' && u.includes('/api/public/score-configs')) return pageFrom(configPages, u)
    if (method === 'GET' && u.includes('/api/public/annotation-queues'))
      return pageFrom(queuePages, u)
    if (method === 'POST' && u.endsWith('/api/public/score-configs')) {
      if (state.failCreate === 'config') return okJson('boom', 500)
      return okJson({
        id: `new-sc-${seq++}`,
        name: body!.name,
        dataType: body!.dataType,
        isArchived: false,
        categories: body!.categories,
      })
    }
    if (method === 'POST' && u.endsWith('/api/public/annotation-queues')) {
      if (state.failCreate === 'queue') return okJson('boom', 500)
      return okJson({
        id: `new-q-${seq++}`,
        name: body!.name,
        scoreConfigIds: body!.scoreConfigIds,
      })
    }
    throw new Error(`unexpected ${method} ${u}`)
  }) as unknown as typeof fetch
  return { impl, calls }
}

const postsTo = (calls: Recorded[], suffix: string): Recorded[] =>
  calls.filter(c => c.method === 'POST' && c.url.endsWith(suffix))

const silent = { log: () => {}, err: () => {} }

afterEach(() => vi.restoreAllMocks())

describe('triage-config — literal contract', () => {
  // These assert LITERAL values so a change to the names/labels/encodings in
  // triage-config.ts is caught here rather than silently tracked by tests that
  // derive their expectations from SCORE_CONFIGS.
  it('pins the queue + verdict names', () => {
    expect(TRIAGE_QUEUE_NAME).toBe('qa-triage')
    expect(VERDICT_SCORE_NAME).toBe('verdict')
    expect(TRIAGE_QUEUE_SCORE_CONFIGS).toEqual(['ground_truth', 'disposition'])
  })

  it('pins the exact score-config matrix (names, dataType, categories + numeric encodings)', () => {
    expect(SCORE_CONFIGS).toEqual([
      {
        name: 'verdict',
        dataType: 'CATEGORICAL',
        categories: [
          { label: 'pass', value: 1 },
          { label: 'unsure', value: 0 },
          { label: 'fail', value: -1 },
        ],
      },
      {
        name: 'ground_truth',
        dataType: 'CATEGORICAL',
        categories: [
          { label: 'should_pass', value: 1 },
          { label: 'unsure', value: 0 },
          { label: 'should_fail', value: -1 },
        ],
      },
      {
        name: 'disposition',
        dataType: 'CATEGORICAL',
        categories: [
          { label: 'acted', value: 1 },
          { label: 'deferred', value: 0 },
          { label: 'dismissed', value: -1 },
        ],
      },
    ])
  })
})

describe('setup.main — provisioning', () => {
  it('fresh tenant: creates all 3 configs + the queue with ground_truth+disposition dims ONLY', async () => {
    const mock = tenantMock({})
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(0)

    // Assert the EXACT POST bodies against LITERAL expected values (not values
    // derived from SCORE_CONFIGS). This locks the wire contract: a changed name /
    // encoding OR any added field (INV-D: no new free-text on the wire) fails here.
    const configPosts = postsTo(mock.calls, '/api/public/score-configs')
    expect(configPosts.map(c => c.body)).toEqual([
      {
        name: 'verdict',
        dataType: 'CATEGORICAL',
        categories: [
          { label: 'pass', value: 1 },
          { label: 'unsure', value: 0 },
          { label: 'fail', value: -1 },
        ],
      },
      {
        name: 'ground_truth',
        dataType: 'CATEGORICAL',
        categories: [
          { label: 'should_pass', value: 1 },
          { label: 'unsure', value: 0 },
          { label: 'should_fail', value: -1 },
        ],
      },
      {
        name: 'disposition',
        dataType: 'CATEGORICAL',
        categories: [
          { label: 'acted', value: 1 },
          { label: 'deferred', value: 0 },
          { label: 'dismissed', value: -1 },
        ],
      },
    ])

    // Creation order is verdict, ground_truth, disposition → new-sc-0/1/2. The queue
    // body is EXACT: references the two HUMAN ids only (new-sc-1, new-sc-2), NOT
    // verdict's new-sc-0, and carries no other field.
    const queuePosts = postsTo(mock.calls, '/api/public/annotation-queues')
    expect(queuePosts.map(c => c.body)).toEqual([
      { name: 'qa-triage', scoreConfigIds: ['new-sc-1', 'new-sc-2'] },
    ])
  })

  it('fully-provisioned + matching → ZERO POSTs (idempotency)', async () => {
    const configs = SCORE_CONFIGS.map(s => goodConfig(s.name))
    const queue: QueueObj = {
      id: 'q1',
      name: TRIAGE_QUEUE_NAME,
      scoreConfigIds: ['ground_truth-id', 'disposition-id'],
    }
    const mock = tenantMock({ configPages: [configs], queuePages: [[queue]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(0)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('partial (verdict matches, others missing) → creates only the two missing configs', async () => {
    const mock = tenantMock({ configPages: [[goodConfig(VERDICT_SCORE_NAME, 'v1')]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(0)
    const configPosts = postsTo(mock.calls, '/api/public/score-configs')
    expect(configPosts.map(c => c.body!.name)).toEqual(['ground_truth', 'disposition'])
    // Queue references the two newly-created human ids.
    const queuePosts = postsTo(mock.calls, '/api/public/annotation-queues')
    expect(queuePosts[0].body!.scoreConfigIds).toEqual(['new-sc-0', 'new-sc-1'])
  })

  it('drift — mismatched categories → non-zero, drift message, zero POSTs', async () => {
    const bad = goodConfig(VERDICT_SCORE_NAME)
    bad.categories = [{ label: 'pass', value: 1 }] // wrong set
    const errs: string[] = []
    const mock = tenantMock({ configPages: [[bad]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, m => errs.push(m))
    expect(code).toBe(1)
    expect(errs.join('\n')).toMatch(/DRIFT/)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('drift — archived score-config (isArchived on the CONFIG) → non-zero, zero POSTs', async () => {
    const archived = goodConfig(VERDICT_SCORE_NAME)
    archived.isArchived = true
    const mock = tenantMock({ configPages: [[archived]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('drift — missing / non-boolean isArchived (strict === false) → non-zero, zero POSTs', async () => {
    for (const bogus of [undefined, null, 0, 'false']) {
      const cfgObj = goodConfig(VERDICT_SCORE_NAME)
      ;(cfgObj as unknown as { isArchived: unknown }).isArchived = bogus
      const mock = tenantMock({ configPages: [[cfgObj]] })
      vi.stubGlobal('fetch', mock.impl)
      const code = await main(creds, silent.log, silent.err)
      expect(code).toBe(1)
      expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
    }
  })

  it('drift — mismatched dataType → non-zero, zero POSTs', async () => {
    const wrongType = goodConfig(VERDICT_SCORE_NAME)
    ;(wrongType as unknown as { dataType: string }).dataType = 'NUMERIC'
    const mock = tenantMock({ configPages: [[wrongType]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('drift — duplicate qa-triage queue (two distinct ids) → non-zero, zero POSTs', async () => {
    const configs = SCORE_CONFIGS.map(s => goodConfig(s.name))
    const q1: QueueObj = {
      id: 'q-a',
      name: TRIAGE_QUEUE_NAME,
      scoreConfigIds: ['ground_truth-id', 'disposition-id'],
    }
    const q2: QueueObj = { ...q1, id: 'q-b' }
    const mock = tenantMock({ configPages: [configs], queuePages: [[q1, q2]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('GET-side ApiError (score-config read returns 500) → non-zero, zero POSTs (fail-closed)', async () => {
    const mock = scripted([{ status: 500, json: 'boom' }])
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('drift — duplicate category label masking a missing one → non-zero, zero POSTs', async () => {
    // [pass, pass, unsure] is length-3 like verdict's [pass, unsure, fail] but drops fail.
    const dup = goodConfig(VERDICT_SCORE_NAME)
    dup.categories = [
      { label: 'pass', value: 1 },
      { label: 'pass', value: 1 },
      { label: 'unsure', value: 0 },
    ]
    const mock = tenantMock({ configPages: [[dup]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('drift — wrong queue scoreConfigIds (queue compared on scoreConfigIds only) → non-zero, zero POSTs', async () => {
    const configs = SCORE_CONFIGS.map(s => goodConfig(s.name))
    const queue: QueueObj = {
      id: 'q1',
      name: TRIAGE_QUEUE_NAME,
      scoreConfigIds: ['verdict-id'], // wrong/incomplete
    }
    const mock = tenantMock({ configPages: [configs], queuePages: [[queue]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('drift — existing queue while a required human config is absent → non-zero, zero POSTs', async () => {
    // verdict present, ground_truth/disposition absent, but the queue exists.
    const queue: QueueObj = { id: 'q1', name: TRIAGE_QUEUE_NAME, scoreConfigIds: ['x', 'y'] }
    const mock = tenantMock({
      configPages: [[goodConfig(VERDICT_SCORE_NAME)]],
      queuePages: [[queue]],
    })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('drift — duplicate same-named config (two DISTINCT ids, one name) → non-zero, zero POSTs', async () => {
    const a = goodConfig(VERDICT_SCORE_NAME, 'v-a')
    const b = goodConfig(VERDICT_SCORE_NAME, 'v-b')
    const mock = tenantMock({ configPages: [[a, b]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('real API error (create POST 500 → ApiError) → non-zero', async () => {
    const mock = tenantMock({ failCreate: 'config' })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
  })

  it('no LANGFUSE_* → exits non-zero, zero fetches', async () => {
    const fetchSpy = vi.fn()
    vi.stubGlobal('fetch', fetchSpy)
    const code = await main({}, silent.log, silent.err)
    expect(code).toBe(1)
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('paginated score-config list (two pages) → all seen, no duplicate create', async () => {
    const configs = SCORE_CONFIGS.map(s => goodConfig(s.name))
    const queue: QueueObj = {
      id: 'q1',
      name: TRIAGE_QUEUE_NAME,
      scoreConfigIds: ['ground_truth-id', 'disposition-id'],
    }
    const mock = tenantMock({
      configPages: [[configs[0], configs[1]], [configs[2]]],
      queuePages: [[queue]],
    })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(0)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('paginated queue list (qa-triage on page 2) → found, not re-created', async () => {
    const configs = SCORE_CONFIGS.map(s => goodConfig(s.name))
    const other: QueueObj = { id: 'q0', name: 'some-other-queue', scoreConfigIds: [] }
    const queue: QueueObj = {
      id: 'q1',
      name: TRIAGE_QUEUE_NAME,
      scoreConfigIds: ['ground_truth-id', 'disposition-id'],
    }
    const mock = tenantMock({ configPages: [configs], queuePages: [[other], [queue]] })
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(0)
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })

  it('propagates a paginator failure (repeated page) to a non-zero exit (fail-closed)', async () => {
    // score-configs GET returns page 1 when page 2 was requested → PaginationError.
    const mock = scripted([pageResp([goodConfig(VERDICT_SCORE_NAME)], 1, 2), pageResp([], 1, 2)])
    vi.stubGlobal('fetch', mock.impl)
    const code = await main(creds, silent.log, silent.err)
    expect(code).toBe(1)
    // No POSTs after the read failed.
    expect(mock.calls.filter(c => c.method === 'POST')).toHaveLength(0)
  })
})

describe('apiGetAllPages — page protocol', () => {
  it('walks two pages and returns the union', async () => {
    const mock = scripted([
      pageResp([{ id: 'a' }, { id: 'b' }], 1, 2),
      pageResp([{ id: 'c' }], 2, 2),
    ])
    vi.stubGlobal('fetch', mock.impl)
    const out = await apiGetAllPages(cfg, '/api/public/score-configs', 'page')
    expect(out.map(o => o.id)).toEqual(['a', 'b', 'c'])
    // Each request carried the tracked page + limit.
    expect(mock.calls[0].url).toContain('page=1')
    expect(mock.calls[1].url).toContain('page=2')
    expect(mock.calls.every(c => c.url.includes('limit=100'))).toBe(true)
  })

  it('a single empty page terminates cleanly', async () => {
    const mock = scripted([pageResp([], 1, 1)])
    vi.stubGlobal('fetch', mock.impl)
    expect(await apiGetAllPages(cfg, '/api/public/annotation-queues', 'page')).toEqual([])
  })

  it('ACCEPTS a fully-duplicate TERMINAL page (guard only fires on continuation)', async () => {
    // page 2 of 2 repeats page 1 entirely — it is terminal, so no-progress does NOT fire.
    const mock = scripted([
      pageResp([{ id: 'a' }, { id: 'b' }], 1, 2),
      pageResp([{ id: 'a' }, { id: 'b' }], 2, 2),
    ])
    vi.stubGlobal('fetch', mock.impl)
    const out = await apiGetAllPages(cfg, '/api/public/score-configs', 'page')
    expect(out.map(o => o.id)).toEqual(['a', 'b'])
  })

  it('preserves caller filters on every page request (MERGES page+limit)', async () => {
    const mock = scripted([pageResp([{ id: 'a' }], 1, 2), pageResp([{ id: 'b' }], 2, 2)])
    vi.stubGlobal('fetch', mock.impl)
    await apiGetAllPages(cfg, '/api/public/score-configs?foo=bar&x=1', 'page')
    for (const c of mock.calls) {
      expect(c.url).toContain('foo=bar')
      expect(c.url).toContain('x=1')
      expect(c.url).toContain('limit=100')
    }
    expect(mock.calls[0].url).toContain('page=1')
    expect(mock.calls[1].url).toContain('page=2')
  })

  it('throws when a non-terminal page adds no new ids (no-progress guard)', async () => {
    // page 1 of 3 = [a]; page 2 of 3 repeats [a] → no new id but more pages remain.
    const mock = scripted([pageResp([{ id: 'a' }], 1, 3), pageResp([{ id: 'a' }], 2, 3)])
    vi.stubGlobal('fetch', mock.impl)
    await expect(apiGetAllPages(cfg, '/api/public/score-configs', 'page')).rejects.toBeInstanceOf(
      PaginationError
    )
  })

  it('throws on a mismatched returned page (stale/replayed page)', async () => {
    const mock = scripted([pageResp([{ id: 'a' }], 1, 3), pageResp([{ id: 'b' }], 1, 3)])
    vi.stubGlobal('fetch', mock.impl)
    await expect(apiGetAllPages(cfg, '/api/public/score-configs', 'page')).rejects.toBeInstanceOf(
      PaginationError
    )
  })

  it('rejects cursor metadata under page mode (protocol mix)', async () => {
    const mock = scripted([{ json: { data: [{ id: 'a' }], meta: { limit: 100, cursor: 'x' } } }])
    vi.stubGlobal('fetch', mock.impl)
    await expect(apiGetAllPages(cfg, '/api/public/score-configs', 'page')).rejects.toBeInstanceOf(
      PaginationError
    )
  })
})

describe('apiGetAllPages — cursor protocol (scores v3)', () => {
  it('follows meta.cursor across pages and stops when the property is absent', async () => {
    const mock = scripted([
      cursorResp([{ id: 's1' }], 'CUR2'),
      cursorResp([{ id: 's2' }]), // no cursor property → terminal
    ])
    vi.stubGlobal('fetch', mock.impl)
    const out = await apiGetAllPages(cfg, '/api/public/v3/scores', 'cursor')
    expect(out.map(o => o.id)).toEqual(['s1', 's2'])
    expect(mock.calls[0].url).not.toContain('cursor=')
    expect(mock.calls[1].url).toContain('cursor=CUR2')
  })

  it('a single-page cursor response (valid meta, no cursor property) is NOT malformed', async () => {
    const mock = scripted([cursorResp([{ id: 's1' }])])
    vi.stubGlobal('fetch', mock.impl)
    expect((await apiGetAllPages(cfg, '/api/public/v3/scores', 'cursor')).map(o => o.id)).toEqual([
      's1',
    ])
  })

  it('throws on a PRESENT null cursor (terminal is the property being ABSENT)', async () => {
    const mock = scripted([cursorResp([{ id: 's1' }], null)])
    vi.stubGlobal('fetch', mock.impl)
    await expect(apiGetAllPages(cfg, '/api/public/v3/scores', 'cursor')).rejects.toBeInstanceOf(
      PaginationError
    )
  })

  it('throws on a repeated cursor (no progress)', async () => {
    const mock = scripted([cursorResp([{ id: 's1' }], 'SAME'), cursorResp([{ id: 's2' }], 'SAME')])
    vi.stubGlobal('fetch', mock.impl)
    await expect(apiGetAllPages(cfg, '/api/public/v3/scores', 'cursor')).rejects.toBeInstanceOf(
      PaginationError
    )
  })

  it('throws on a FRESH cursor that adds no new ids (fabricated-continuation loop)', async () => {
    // Distinct cursors each page, but page 2 repeats page 1's only id → no progress.
    const mock = scripted([cursorResp([{ id: 's1' }], 'CUR2'), cursorResp([{ id: 's1' }], 'CUR3')])
    vi.stubGlobal('fetch', mock.impl)
    await expect(apiGetAllPages(cfg, '/api/public/v3/scores', 'cursor')).rejects.toBeInstanceOf(
      PaginationError
    )
  })

  it('ACCEPTS a fully-duplicate TERMINAL cursor page (guard only fires on continuation)', async () => {
    // page 2 repeats page 1's id but is terminal (no cursor) → no-progress does NOT fire.
    const mock = scripted([cursorResp([{ id: 's1' }], 'CUR2'), cursorResp([{ id: 's1' }])])
    vi.stubGlobal('fetch', mock.impl)
    expect((await apiGetAllPages(cfg, '/api/public/v3/scores', 'cursor')).map(o => o.id)).toEqual([
      's1',
    ])
  })

  it('throws on a present non-string / empty cursor', async () => {
    for (const bad of [123, '']) {
      const mock = scripted([{ json: { data: [{ id: 's1' }], meta: { limit: 100, cursor: bad } } }])
      vi.stubGlobal('fetch', mock.impl)
      await expect(apiGetAllPages(cfg, '/api/public/v3/scores', 'cursor')).rejects.toBeInstanceOf(
        PaginationError
      )
    }
  })

  it('rejects page metadata under cursor mode (protocol mix)', async () => {
    const mock = scripted([pageResp([{ id: 's1' }], 1, 1)])
    vi.stubGlobal('fetch', mock.impl)
    await expect(apiGetAllPages(cfg, '/api/public/v3/scores', 'cursor')).rejects.toBeInstanceOf(
      PaginationError
    )
  })

  it('rejects a page-only totalItems under cursor mode (no silent truncation)', async () => {
    // {limit, totalItems} has no cursor property → would otherwise look terminal, but
    // totalItems is page-protocol metadata and must be rejected as a protocol mix.
    const mock = scripted([{ json: { data: [{ id: 's1' }], meta: { limit: 100, totalItems: 1 } } }])
    vi.stubGlobal('fetch', mock.impl)
    await expect(apiGetAllPages(cfg, '/api/public/v3/scores', 'cursor')).rejects.toBeInstanceOf(
      PaginationError
    )
  })
})

describe('apiGetAllPages — query preservation + dedup + malformed', () => {
  it('MERGES pagination params without clobbering the caller filters (every page)', async () => {
    const mock = scripted([cursorResp([{ id: 's1' }], 'CUR2'), cursorResp([{ id: 's2' }])])
    vi.stubGlobal('fetch', mock.impl)
    await apiGetAllPages(
      cfg,
      '/api/public/v3/scores?name=verdict&dataType=CATEGORICAL&fields=subject',
      'cursor'
    )
    for (const c of mock.calls) {
      expect(c.url).toContain('name=verdict')
      expect(c.url).toContain('dataType=CATEGORICAL')
      expect(c.url).toContain('fields=subject')
    }
  })

  it('dedups overlapping pages by id (repeated id appears once, still terminates)', async () => {
    const mock = scripted([
      pageResp([{ id: 'a' }, { id: 'b' }], 1, 2),
      pageResp([{ id: 'b' }, { id: 'c' }], 2, 2), // b repeated + c new
    ])
    vi.stubGlobal('fetch', mock.impl)
    const out = await apiGetAllPages(cfg, '/api/public/score-configs', 'page')
    expect(out.map(o => o.id)).toEqual(['a', 'b', 'c'])
  })

  it('throws on missing data array', async () => {
    const mock = scripted([
      { json: { meta: { page: 1, limit: 100, totalItems: 0, totalPages: 1 } } },
    ])
    vi.stubGlobal('fetch', mock.impl)
    await expect(apiGetAllPages(cfg, '/api/public/score-configs', 'page')).rejects.toBeInstanceOf(
      PaginationError
    )
  })

  it('throws on missing/malformed meta (null, array, scalar)', async () => {
    for (const meta of [null, [], 7]) {
      const mock = scripted([{ json: { data: [], meta } }])
      vi.stubGlobal('fetch', mock.impl)
      await expect(apiGetAllPages(cfg, '/api/public/score-configs', 'page')).rejects.toBeInstanceOf(
        PaginationError
      )
    }
  })

  it('throws on invalid meta.page / meta.totalPages', async () => {
    const bad = [
      { data: [{ id: 'a' }], meta: { page: 0, limit: 100, totalItems: 1, totalPages: 1 } },
      { data: [{ id: 'a' }], meta: { page: 1, limit: 100, totalItems: 1, totalPages: -1 } },
      { data: [{ id: 'a' }], meta: { page: '1', limit: 100, totalItems: 1, totalPages: 1 } },
    ]
    for (const json of bad) {
      const mock = scripted([{ json }])
      vi.stubGlobal('fetch', mock.impl)
      await expect(apiGetAllPages(cfg, '/api/public/score-configs', 'page')).rejects.toBeInstanceOf(
        PaginationError
      )
    }
  })

  it('throws on an item lacking a valid string id', async () => {
    for (const item of [{ name: 'x' }, { id: 42 }, { id: '' }]) {
      const mock = scripted([pageResp([item], 1, 1)])
      vi.stubGlobal('fetch', mock.impl)
      await expect(apiGetAllPages(cfg, '/api/public/score-configs', 'page')).rejects.toBeInstanceOf(
        PaginationError
      )
    }
  })
})

describe('setup.ts — import without side effects', () => {
  it('importing the module runs no network / logging / exit — discriminates the import.meta.main guard', async () => {
    // If the guard were removed, `void main()` would run on import: with creds absent
    // main() SYNCHRONOUSLY logs a config error (console.error) before any await; with
    // creds present it SYNCHRONOUSLY calls fetch before its first await. Asserting
    // none of those fire (nor process.exit / a changed exitCode) makes this test
    // actually fail if the guard is dropped — regardless of ambient LANGFUSE_* env.
    vi.resetModules()
    const exitCodeBefore = process.exitCode
    const fetchSpy = vi.fn()
    vi.stubGlobal('fetch', fetchSpy)
    const logSpy = vi.spyOn(console, 'log').mockImplementation(() => {})
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const exitSpy = vi.spyOn(process, 'exit').mockImplementation((() => undefined) as never)
    const mod = await import('./setup')
    expect(typeof mod.main).toBe('function')
    expect(fetchSpy).not.toHaveBeenCalled()
    expect(logSpy).not.toHaveBeenCalled()
    expect(errSpy).not.toHaveBeenCalled()
    expect(exitSpy).not.toHaveBeenCalled()
    expect(process.exitCode).toBe(exitCodeBefore)
  })
})
