import { execFileSync, spawn } from 'node:child_process'
import { createServer } from 'node:http'
import type { AddressInfo } from 'node:net'
import path from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { main } from './backfill'
import { TRIAGE_QUEUE_NAME, VERDICT_SCORE_NAME } from './triage-config'

// --- fixtures ---------------------------------------------------------------

const ENV = {
  LANGFUSE_HOST: 'http://lf',
  LANGFUSE_PUBLIC_KEY: 'p',
  LANGFUSE_SECRET_KEY: 's',
}

type Verdict = 'pass' | 'fail' | 'unsure'

// A root observation as `GET /api/public/v2/observations` returns it with fields
// `core,io,trace_context` (arc §2, S2): `input`/`output` are raw strings, and `tags`
// are the trace's tags.
interface RootObs {
  id: string
  traceId: string
  parentObservationId: string | null
  startTime: string
  traceName: string
  tags: string[]
  input: string | null
  output: string | null
}
interface ScoreObj {
  id: string
  name: string
  value: string
  dataType: string
  subject: { kind: string; id: string }
}
interface QueueObj {
  id: string
  name: string
}

// A graded item-trace's output shape (PerItemVerdict).
const verdictOutput = (verdict: Verdict, citation = 'c', critique = 'k'): unknown => ({
  itemIndex: 0,
  verdict,
  citation,
  critique,
})

// The serialization v2 returns for the value a legacy trace body's `output` carried.
const serialized = (output: unknown): string | null =>
  output === undefined ? null : JSON.stringify(output)

// A historic trace's root (arc §2, S1): a string trace id and an observation id of
// `t-<traceId>`. Most tests below build their traces this way.
const trace = (traceId: string, tags: string[], output?: unknown): RootObs => ({
  id: `t-${traceId}`,
  traceId,
  parentObservationId: null,
  startTime: '2026-07-17T15:16:39.000Z',
  traceName: 'judge-synthetic',
  tags,
  input: null,
  output: serialized(output),
})

// A new trace's root (arc §2, S1): a 32-hex trace id and its own 16-hex span id.
const newRoot = (traceId: string, spanId: string, tags: string[], output?: unknown): RootObs => ({
  ...trace(traceId, tags, output),
  id: spanId,
})

// A non-root observation of the same trace (the generation PR3 ships). v2's
// `traceTags` filter matches it too, so only the root filter keeps it out.
const childObs = (root: RootObs, spanId: string): RootObs => ({
  ...root,
  id: spanId,
  parentObservationId: root.id,
  output: serialized('raw model completion'),
})

// A trace-level verdict score: the trace id lives in `subject.id` (kind === 'trace').
const traceScore = (traceId: string, value: string): ScoreObj => ({
  id: `sc-${traceId}-${value}`,
  name: VERDICT_SCORE_NAME,
  value,
  dataType: 'CATEGORICAL',
  subject: { kind: 'trace', id: traceId },
})

const triageQueue: QueueObj = { id: 'q-triage', name: TRIAGE_QUEUE_NAME }

interface MockOpts {
  // Every observation the fake holds; the `trace`/`newRoot` helpers build roots.
  traces?: RootObs[]
  tracesPerPage?: number
  tracesError?: boolean
  tracesMalformed?: boolean
  // Answer the observations read as if the server dropped the `filter` param.
  ignoreFilter?: boolean
  // scores/items accept raw shapes so a test can inject a spec-invalid row.
  scores?: unknown[]
  scoresPerPage?: number
  scoresError?: boolean
  projects?: { id: string }[]
  projectsError?: boolean
  queues?: QueueObj[]
  queuesPerPage?: number
  queueError?: boolean
  items?: unknown[]
  itemsPerPage?: number
  itemsError?: boolean
  itemsMalformed?: boolean
  failItemPost?: boolean
}

// A fake Langfuse's reply to one request.
interface FakeReply {
  status: number
  body: string
}

// One fake Langfuse router. The function-level tests reach it through `fetchImpl` (a
// stubbed global fetch); the command-level tests reach it over HTTP (`runCommand`).
function mock(opts: MockOpts = {}): {
  fetchImpl: typeof fetch
  handle: (method: string, url: URL, body: string | undefined) => FakeReply
  tracesReqs: URLSearchParams[]
  scoresReqs: URLSearchParams[]
  // The EXACT POST bodies (to prove INV-D: identifier/enum-only wire fields) + their queues.
  itemPosts: Array<Record<string, unknown>>
  postQueueIds: string[]
  calls: number
} {
  const tracesReqs: URLSearchParams[] = []
  const scoresReqs: URLSearchParams[] = []
  const itemPosts: Array<Record<string, unknown>> = []
  const postQueueIds: string[] = []
  const state = { calls: 0 }

  const okText = (obj: unknown): FakeReply => ({ status: 200, body: JSON.stringify(obj) })
  const err = (status: number, msg: string): FakeReply => ({ status, body: msg })

  // Page protocol: slice `all` to the requested page, emit utilsMetaResponse.
  const page = (all: unknown[], q: URLSearchParams, per: number): FakeReply => {
    const requested = Number(q.get('page') ?? '1')
    const totalPages = Math.max(1, Math.ceil(all.length / per))
    const start = (requested - 1) * per
    return okText({
      data: all.slice(start, start + per),
      meta: { page: requested, limit: per, totalItems: all.length, totalPages },
    })
  }
  // Cursor protocol: `cursor` query is a numeric offset; omit meta.cursor on the last page.
  const cursorPage = (all: unknown[], q: URLSearchParams, per: number): FakeReply => {
    const offset = Number(q.get('cursor') ?? '0')
    const slice = all.slice(offset, offset + per)
    const next = offset + per
    const meta: Record<string, unknown> = { limit: per }
    if (next < all.length) meta.cursor = String(next)
    return okText({ data: slice, meta })
  }

  // Observations v2: `cursor` is a numeric offset; `meta: {}` on the last page, and
  // never a `limit`.
  const v2Page = (all: unknown[], q: URLSearchParams, per: number): FakeReply => {
    const offset = Number(q.get('cursor') ?? '0')
    const next = offset + per
    return okText({
      data: all.slice(offset, next),
      meta: next < all.length ? { cursor: String(next) } : {},
    })
  }

  const handle = (method: string, url: URL, body: string | undefined): FakeReply => {
    state.calls++
    const pathname = url.pathname
    const q = url.searchParams
    const json = (): Record<string, unknown> => JSON.parse(String(body)) as Record<string, unknown>

    if (pathname === '/api/public/v2/observations' && method === 'GET') {
      tracesReqs.push(new URLSearchParams(q))
      if (opts.tracesError === true) return err(500, 'observations boom')
      if (opts.tracesMalformed === true) return okText({ meta: {} })
      // The one filter shape the backfill sends: all of the given trace tags.
      const filter = JSON.parse(q.get('filter') ?? '[]') as Array<Record<string, unknown>>
      const tagFilter = filter.find(f => f.column === 'traceTags')
      if (
        filter.length !== 1 ||
        tagFilter?.type !== 'arrayOptions' ||
        tagFilter.operator !== 'all of' ||
        !Array.isArray(tagFilter.value)
      ) {
        return err(400, 'invalid filter')
      }
      const wanted = tagFilter.value as string[]
      const rootsOnly = q.get('isRootObservation') === 'true'
      const all = (opts.traces ?? []).filter(
        t =>
          (opts.ignoreFilter === true || wanted.every(w => t.tags.includes(w))) &&
          (!rootsOnly || t.parentObservationId === null)
      )
      return v2Page(all, q, opts.tracesPerPage ?? 100)
    }
    if (pathname === '/api/public/v3/scores' && method === 'GET') {
      scoresReqs.push(new URLSearchParams(q))
      if (opts.scoresError === true) return err(500, 'scores boom')
      return cursorPage(opts.scores ?? [], q, opts.scoresPerPage ?? 100)
    }
    if (pathname === '/api/public/projects' && method === 'GET') {
      if (opts.projectsError === true) return err(500, 'projects boom')
      return okText({ data: opts.projects ?? [{ id: 'proj-1' }] })
    }
    if (pathname === '/api/public/annotation-queues' && method === 'GET') {
      if (opts.queueError === true) return err(500, 'queue boom')
      return page(opts.queues ?? [triageQueue], q, opts.queuesPerPage ?? 100)
    }
    const itemsMatch = /^\/api\/public\/annotation-queues\/([^/]+)\/items$/.exec(pathname)
    if (itemsMatch) {
      if (method === 'GET') {
        if (opts.itemsError === true) return err(500, 'items boom')
        if (opts.itemsMalformed === true)
          return okText({ meta: { page: 1, limit: 100, totalItems: 0, totalPages: 1 } })
        return page(opts.items ?? [], q, opts.itemsPerPage ?? 100)
      }
      if (method === 'POST') {
        const b = json()
        itemPosts.push(b)
        postQueueIds.push(itemsMatch[1])
        if (opts.failItemPost === true) return err(500, 'post boom')
        return okText({ id: 'new-item', ...b, status: 'PENDING' })
      }
    }
    // Any route/method the CLI is NOT expected to touch (e.g. a PATCH reopen or a direct
    // score write) reddens the test — a silent 200 would make the no-write tests toothless.
    throw new Error(`unexpected fetch: ${method} ${pathname}`)
  }

  const fetchImpl = (async (url: string | URL, init?: { method?: string; body?: unknown }) => {
    const reply = handle(
      init?.method ?? 'GET',
      new URL(String(url)),
      init?.body === undefined ? undefined : String(init.body)
    )
    return {
      ok: reply.status >= 200 && reply.status < 300,
      status: reply.status,
      text: async () => reply.body,
    } as Response
  }) as unknown as typeof fetch

  return {
    fetchImpl,
    handle,
    tracesReqs,
    scoresReqs,
    itemPosts,
    postQueueIds,
    get calls() {
      return state.calls
    },
  }
}

// Capture stdout/stderr lines a run emits.
function sinks(): {
  log: (m: string) => void
  errlog: (m: string) => void
  out: string[]
  er: string[]
} {
  const out: string[] = []
  const er: string[] = []
  return { log: m => out.push(m), errlog: m => er.push(m), out, er }
}

const RUN_ID = '20260717T151639Z'

// The request contract of the candidate read (arc §2, S2; the plan's tag filter): root
// observations, fields `core,io,trace_context`, and one `traceTags` "all of" filter.
// Returns the filter's tags.
function rootReadTags(q: URLSearchParams): string[] {
  expect(q.get('isRootObservation')).toBe('true')
  expect(q.get('fields')).toBe('core,io,trace_context')
  const filter = JSON.parse(q.get('filter') ?? 'null') as unknown
  expect(filter).toEqual([
    { type: 'arrayOptions', column: 'traceTags', operator: 'all of', value: expect.any(Array) },
  ])
  return (filter as Array<{ value: string[] }>)[0].value
}

// --- command level: the process `make qa-fn-backfill` runs ---------------------

// The Makefile target runs `cd frontend && bun run tests/tours/judge/export/backfill.ts
// <BEHAVIOR> [<TRACE> | --round <ROUND>]`. `runCommand` runs that command as a child
// process against a fake Langfuse served over HTTP on loopback, with only the fake host
// and dummy keys in its environment. `--no-env-file` stops bun from adding frontend/'s
// .env files to that environment; it changes nothing else about the command.
const FRONTEND_DIR = path.resolve(__dirname, '../../../..')
const BACKFILL_SCRIPT = 'tests/tours/judge/export/backfill.ts'
const BUN = execFileSync('sh', ['-c', 'command -v bun'], { encoding: 'utf8' }).trim()
const FAKE_PUBLIC_KEY = 'pk-lf-fake'
const FAKE_SECRET_KEY = 'sk-lf-fake'
const FAKE_AUTH = 'Basic ' + Buffer.from(`${FAKE_PUBLIC_KEY}:${FAKE_SECRET_KEY}`).toString('base64')

// One request the command transported to the fake.
interface Transported {
  method: string
  url: URL
  body: string | undefined
}
interface CommandRun {
  code: number | null
  stdout: string
  stderr: string
  // The fake's base URL, which deep links start with.
  host: string
  requests: Transported[]
}

async function runCommand(args: string[], opts: MockOpts): Promise<CommandRun> {
  const m = mock(opts)
  const requests: Transported[] = []
  const auths: Array<string | undefined> = []
  const unexpected: string[] = []
  const server = createServer((req, res) => {
    const chunks: Buffer[] = []
    req.on('data', (c: Buffer) => chunks.push(c))
    req.on('end', () => {
      const method = req.method ?? 'GET'
      const url = new URL(req.url ?? '/', 'http://fake')
      const body = chunks.length > 0 ? Buffer.concat(chunks).toString('utf8') : undefined
      requests.push({ method, url, body })
      auths.push(req.headers.authorization)
      let reply: FakeReply
      try {
        reply = m.handle(method, url, body)
      } catch (e) {
        unexpected.push(e instanceof Error ? e.message : String(e))
        reply = { status: 500, body: 'unexpected request' }
      }
      res.writeHead(reply.status, { 'Content-Type': 'application/json' }).end(reply.body)
    })
  })
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  const host = `http://127.0.0.1:${(server.address() as AddressInfo).port}`
  try {
    const child = spawn(BUN, ['--no-env-file', 'run', BACKFILL_SCRIPT, ...args], {
      cwd: FRONTEND_DIR,
      // Next's types require NODE_ENV on ProcessEnv; the child deliberately has none.
      env: {
        LANGFUSE_HOST: host,
        LANGFUSE_PUBLIC_KEY: FAKE_PUBLIC_KEY,
        LANGFUSE_SECRET_KEY: FAKE_SECRET_KEY,
      } as Partial<NodeJS.ProcessEnv> as NodeJS.ProcessEnv,
    })
    let stdout = ''
    let stderr = ''
    child.stdout.on('data', (d: Buffer) => (stdout += d.toString('utf8')))
    child.stderr.on('data', (d: Buffer) => (stderr += d.toString('utf8')))
    const code = await new Promise<number | null>((resolve, reject) => {
      child.on('error', reject)
      child.on('close', resolve)
    })
    // Every request was one the fake serves, authenticated with the dummy keys.
    expect(unexpected).toEqual([])
    expect(auths.every(a => a === FAKE_AUTH)).toBe(true)
    return { code, stdout, stderr, host, requests }
  } finally {
    server.closeAllConnections()
    await new Promise<void>(resolve => server.close(() => resolve()))
  }
}

// The query of each request the run made to `pathname`, in order.
const gets = (run: CommandRun, pathname: string): URLSearchParams[] =>
  run.requests
    .filter(r => r.method === 'GET' && r.url.pathname === pathname)
    .map(r => r.url.searchParams)

// The decoded body of each write the run made.
const writes = (run: CommandRun): Array<{ method: string; path: string; body: unknown }> =>
  run.requests
    .filter(r => r.method !== 'GET')
    .map(r => ({
      method: r.method,
      path: r.url.pathname,
      body: r.body === undefined ? undefined : (JSON.parse(r.body) as unknown),
    }))

const OBSERVATIONS_V2 = '/api/public/v2/observations'

afterEach(() => vi.restoreAllMocks())

// --- list: covering-PASS candidates + deep-links ----------------------------

describe('list — covering-PASS candidates + deep-links', () => {
  it('lists only covering-PASS traces with a resolvable deep-link, paginating traces AND scores', async () => {
    // t1: scored pass → candidate; t2: scored fail → excluded; t3: zero-score output-PASS
    // → candidate (fallback). Force multi-page traces AND cursor-paged scores.
    const traces = [
      trace('t1', ['behavior:CON-042'], verdictOutput('pass', 'row gone', 'looks right')),
      trace('t2', ['behavior:CON-042'], verdictOutput('fail')),
      trace('t3', ['behavior:CON-042'], verdictOutput('pass', 'cite3', 'crit3')),
    ]
    const scores = [traceScore('t1', 'pass'), traceScore('t2', 'fail')]
    const m = mock({ traces, tracesPerPage: 1, scores, scoresPerPage: 1 })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    const shipped = s.out.join('\n')
    // Candidates t1 + t3; NOT t2.
    expect(shipped).toContain('t1')
    expect(shipped).toContain('t3')
    expect(shipped).not.toMatch(/(^|\s)t2(\s|$)/)
    // Deep-link uses the resolved project id.
    expect(shipped).toContain('http://lf/project/proj-1/traces/t1')
    expect(shipped).toContain('cite: row gone')
    expect(shipped).toContain('critique: looks right')
    // Roots were paginated (3 requests over 3 cursor pages), each carrying the root read.
    expect(m.tracesReqs.length).toBeGreaterThanOrEqual(3)
    for (const q of m.tracesReqs) expect(rootReadTags(q)).toEqual(['behavior:CON-042'])
    // EVERY scores request retained name/dataType/fields=subject and carried NO value filter.
    expect(m.scoresReqs.length).toBeGreaterThanOrEqual(2)
    for (const q of m.scoresReqs) {
      expect(q.get('name')).toBe(VERDICT_SCORE_NAME)
      expect(q.get('dataType')).toBe('CATEGORICAL')
      expect(q.get('fields')).toBe('subject')
      expect(q.has('value')).toBe(false)
    }
  })

  it('ignores a non-trace (observation) score subject even when its id collides with a trace', async () => {
    // t1 output FAIL → only an OBSERVATION-subject pass score references its id. If that
    // were wrongly joined, t1 would look like one scored pass → candidate. Correct: the
    // observation score is ignored, t1 stays scoreless → output FAIL → NOT a candidate.
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('fail'))],
      scores: [
        {
          id: 'sc-obs',
          name: VERDICT_SCORE_NAME,
          value: 'pass',
          dataType: 'CATEGORICAL',
          subject: { kind: 'observation', id: 't1', traceId: 't1' },
        },
      ],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).toBe(4) // no covering pass
  })
})

// --- enqueue happy path -----------------------------------------------------

describe('enqueue — happy path', () => {
  it('enqueues a valid candidate with EXACTLY {objectId, objectType:TRACE} and exits 0', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    // The wire body is identifier/enum-only — no free-text, no extra fields (INV-D).
    expect(m.itemPosts).toEqual([{ objectId: 't1', objectType: 'TRACE' }])
    expect(m.postQueueIds).toEqual(['q-triage'])
    expect(s.out.join('\n')).toContain('enqueued t1')
  })
})

// --- enqueue refuses a non-member -------------------------------------------

describe('enqueue — refuses a non-member trace', () => {
  it('refuses a traceId not in the candidate set (wrong behavior) and does NOT POST', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 'not-a-real-trace'], ENV, s.log, s.errlog)
    expect(code).not.toBe(0)
    expect(m.itemPosts).toHaveLength(0)
    expect(s.er.join('\n')).toMatch(/not a covering-PASS candidate/)
  })

  it('refuses a trace that covers the behavior but is currently NON-pass', async () => {
    const m = mock({
      traces: [trace('t9', ['behavior:CON-042'], verdictOutput('fail'))],
      scores: [traceScore('t9', 'fail')],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't9'], ENV, s.log, s.errlog)
    expect(code).not.toBe(0)
    expect(m.itemPosts).toHaveLength(0)
  })
})

// --- three distinct not-found outcomes --------------------------------------

describe('three distinct outcomes: unknown / zero-traces / no-covering-pass', () => {
  it('unknown behavior (not in registry) → distinct message + code 2, no query', async () => {
    const m = mock()
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['ZZZ-999'], ENV, s.log, s.errlog)
    expect(code).toBe(2)
    expect(s.er.join('\n')).toMatch(/unknown behavior/)
    expect(m.calls).toBe(0) // rejected before any API call
  })

  it('resolves an INTENT_CATALOG id (DSH-010), proving the intent half of the registry', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:DSH-010'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['DSH-010'], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    expect(s.out.join('\n')).toContain('t1')
  })

  it('valid behavior, zero traces → distinct message + code 3', async () => {
    const m = mock({ traces: [] })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['DSH-004'], ENV, s.log, s.errlog)
    expect(code).toBe(3)
    expect(s.er.join('\n')).toMatch(/no traces tagged behavior:DSH-004/)
    expect(s.er.join('\n')).toMatch(/coverage gap/)
  })

  it('valid behavior, traces but no covering PASS → distinct message + code 4', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('fail'))],
      scores: [traceScore('t1', 'fail')],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).toBe(4)
    expect(s.er.join('\n')).toMatch(/none is a covering PASS/)
  })
})

// --- enqueue duplicate-safe on an existing PENDING item ---------------------

describe('enqueue — duplicate-safe on an existing PENDING item', () => {
  it('a trace already queued and PENDING is a no-op "already queued (pending)", exit 0, no POST', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
      items: [{ id: 'e1', objectId: 't1', objectType: 'TRACE', status: 'PENDING' }],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    expect(m.itemPosts).toHaveLength(0)
    expect(s.out.join('\n')).toMatch(/already queued \(pending\)/)
  })

  it('finds an existing PENDING item on a LATER item page (paginated dedup), no dup POST', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
      items: [
        { id: 'e0', objectId: 'zzz', objectType: 'TRACE', status: 'PENDING' },
        { id: 'e1', objectId: 't1', objectType: 'TRACE', status: 'PENDING' }, // on page 2
      ],
      itemsPerPage: 1,
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    expect(m.itemPosts).toHaveLength(0)
  })

  it('a same-id OBSERVATION item does NOT satisfy the TRACE check → still POSTs', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
      items: [{ id: 'e1', objectId: 't1', objectType: 'OBSERVATION', status: 'PENDING' }],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    expect(m.itemPosts).toHaveLength(1)
  })
})

// --- --round dispatch + strict arity ----------------------------------------

describe('--round dispatch + strict arity', () => {
  it('a valid runId narrows the trace query by the runId: tag', async () => {
    const m = mock({
      traces: [
        trace('t1', ['behavior:CON-042', `runId:${RUN_ID}`], verdictOutput('pass')),
        trace('t2', ['behavior:CON-042'], verdictOutput('pass')), // other round
      ],
      scores: [traceScore('t1', 'pass'), traceScore('t2', 'pass')],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', '--round', RUN_ID], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    expect(rootReadTags(m.tracesReqs[0])).toEqual(['behavior:CON-042', `runId:${RUN_ID}`])
    const shipped = s.out.join('\n')
    expect(shipped).toContain('t1')
    expect(shipped).not.toMatch(/(^|\s)t2(\s|$)/)
  })

  it('a valid gitSha narrows the trace query by the gitSha: tag', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042', 'gitSha:abc1234'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', '--round', 'abc1234'], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    expect(rootReadTags(m.tracesReqs[0])).toEqual(['behavior:CON-042', 'gitSha:abc1234'])
  })

  it('a --round value that is neither a run-id nor a git sha → usage error, no query', async () => {
    for (const bad of ['20269999T999999Z', 'not-a-sha!', 'GHIJKLM']) {
      const m = mock()
      vi.stubGlobal('fetch', m.fetchImpl)
      const s = sinks()
      const code = await main(['CON-042', '--round', bad], ENV, s.log, s.errlog)
      expect(code).toBe(2)
      expect(m.calls).toBe(0)
      vi.restoreAllMocks()
    }
  })

  it('--round with no value → usage error, no query', async () => {
    const m = mock()
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', '--round'], ENV, s.log, s.errlog)
    expect(code).toBe(2)
    expect(m.calls).toBe(0)
  })

  it('rejects any invocation that is not exactly a recognized form, before any query', async () => {
    const malformed = [
      ['CON-042', '--round', 'abc1234', 'extra'], // trailing arg after --round
      ['CON-042', 't1', '--round', 'abc1234'], // enqueue + round mixed
      ['CON-042', 't1', 'extra'], // enqueue with a trailing arg
      ['CON-042', '--unknown-flag'], // unknown flag in the enqueue slot
    ]
    for (const argv of malformed) {
      const m = mock()
      vi.stubGlobal('fetch', m.fetchImpl)
      const s = sinks()
      const code = await main(argv, ENV, s.log, s.errlog)
      expect(code, `argv=${argv.join(' ')}`).toBe(2)
      expect(m.calls, `argv=${argv.join(' ')}`).toBe(0)
      vi.restoreAllMocks()
    }
  })
})

// --- mixed-population score-presence classification -------------------------

describe('mixed-population score-presence classification', () => {
  it('candidates are exactly {scored-pass, scoreless output-PASS}; scored fail/unsure excluded despite output PASS', async () => {
    const traces = [
      trace('scored-pass', ['behavior:CON-042'], verdictOutput('pass', 'a', 'aa')),
      trace('scoreless-pass', ['behavior:CON-042'], verdictOutput('pass', 'b', 'bb')),
      trace('scored-fail', ['behavior:CON-042'], verdictOutput('pass', 'c', 'cc')), // output PASS...
      trace('scored-unsure', ['behavior:CON-042'], verdictOutput('pass', 'd', 'dd')), // ...but scored
      trace('scoreless-fail', ['behavior:CON-042'], verdictOutput('fail', 'e', 'ee')),
    ]
    const scores = [
      traceScore('scored-pass', 'pass'),
      traceScore('scored-fail', 'fail'),
      traceScore('scored-unsure', 'unsure'),
    ]
    const m = mock({ traces, scores })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    const shipped = s.out.join('\n')
    expect(shipped).toContain('scored-pass')
    expect(shipped).toContain('scoreless-pass')
    expect(shipped).not.toContain('scored-fail')
    expect(shipped).not.toContain('scored-unsure')
    expect(shipped).not.toContain('scoreless-fail')
    expect(shipped).toMatch(/2 covering-PASS candidate/)
  })

  it('a trace with MULTIPLE verdict scores → ambiguous: reports the trace id, exits non-zero, lists nothing', async () => {
    const m = mock({
      traces: [trace('t-amb', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t-amb', 'pass'), traceScore('t-amb', 'fail')],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(s.er.join('\n')).toMatch(/ambiguous/)
    expect(s.er.join('\n')).toContain('t-amb')
    expect(s.out.join('\n')).not.toMatch(/covering-PASS candidate/)
  })

  it('ambiguity also blocks enqueue (never picks one)', async () => {
    const m = mock({
      traces: [trace('t-amb', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t-amb', 'pass'), traceScore('t-amb', 'unsure')],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't-amb'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(m.itemPosts).toHaveLength(0)
  })
})

// --- existing-item report — no PATCH/reopen ---------------------------------

describe('existing-item report — no PATCH/reopen', () => {
  it('an existing COMPLETED item → reported "reopen in UI" + non-zero, no POST, no ground-truth label', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
      items: [{ id: 'e1', objectId: 't1', objectType: 'TRACE', status: 'COMPLETED' }],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).not.toBe(0)
    expect(m.itemPosts).toHaveLength(0)
    const er = s.er.join('\n')
    expect(er).toMatch(/already triaged \(COMPLETED\)/)
    expect(er).toMatch(/reopen it in the Langfuse/)
    // The CLI never queries or prints a ground_truth label.
    expect((s.out.join('\n') + er).toLowerCase()).not.toContain('should_fail')
  })

  it('MULTIPLE matching items (mixed statuses) → all reported + non-zero, never picks one', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
      items: [
        { id: 'e1', objectId: 't1', objectType: 'TRACE', status: 'PENDING' },
        { id: 'e2', objectId: 't1', objectType: 'TRACE', status: 'COMPLETED' },
      ],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).not.toBe(0)
    expect(m.itemPosts).toHaveLength(0)
    const er = s.er.join('\n')
    expect(er).toMatch(/2 matching queue items/)
    expect(er).toContain('e1')
    expect(er).toContain('e2')
  })
})

// --- deep-link resolution ---------------------------------------------------

describe('deep-link resolution', () => {
  const candidate = {
    traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
    scores: [traceScore('t1', 'pass')],
  }

  it('LANGFUSE_PROJECT_ID env → used directly, no projects call', async () => {
    const m = mock(candidate)
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(
      ['CON-042'],
      { ...ENV, LANGFUSE_PROJECT_ID: 'env-proj' },
      s.log,
      s.errlog
    )
    expect(code).toBe(0)
    expect(s.out.join('\n')).toContain('http://lf/project/env-proj/traces/t1')
  })

  it('no env → the API sole project is used', async () => {
    const m = mock({ ...candidate, projects: [{ id: 'only-1' }] })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).toBe(0)
    expect(s.out.join('\n')).toContain('http://lf/project/only-1/traces/t1')
  })

  it('zero projects → fail-closed', async () => {
    const m = mock({ ...candidate, projects: [] })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).not.toBe(0)
    expect(s.er.join('\n')).toMatch(/LANGFUSE_PROJECT_ID/)
  })

  it('multiple projects → fail-closed', async () => {
    const m = mock({ ...candidate, projects: [{ id: 'a' }, { id: 'b' }] })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).not.toBe(0)
  })

  it('a projects API error → fail-closed', async () => {
    const m = mock({ ...candidate, projectsError: true })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).not.toBe(0)
  })

  it('missing creds → fail-closed usage exit', async () => {
    const m = mock(candidate)
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], {}, s.log, s.errlog)
    expect(code).toBe(2)
    expect(m.calls).toBe(0)
  })
})

// --- fail-closed write failures ---------------------------------------------

describe('fail-closed write failures', () => {
  const cand = {
    traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
    scores: [traceScore('t1', 'pass')],
  }

  it('missing qa-triage queue → non-zero, no POST', async () => {
    const m = mock({ ...cand, queues: [] })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).not.toBe(0)
    expect(m.itemPosts).toHaveLength(0)
    expect(s.er.join('\n')).toMatch(/expected exactly 1/)
  })

  it('ambiguous (multiple) qa-triage queue → non-zero, no POST', async () => {
    const m = mock({ ...cand, queues: [triageQueue, { id: 'q2', name: TRIAGE_QUEUE_NAME }] })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).not.toBe(0)
    expect(m.itemPosts).toHaveLength(0)
  })

  it('a queue-list GET error → non-zero', async () => {
    const m = mock({ ...cand, queueError: true })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(m.itemPosts).toHaveLength(0)
  })

  it('an existing-items GET error → non-zero', async () => {
    const m = mock({ ...cand, itemsError: true })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(m.itemPosts).toHaveLength(0)
  })

  it('a PaginationError listing candidate traces → non-zero (no list)', async () => {
    const m = mock({ ...cand, tracesMalformed: true })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(s.er.join('\n')).toMatch(/PaginationError/)
  })

  it('a PaginationError reading existing items → non-zero (no POST)', async () => {
    const m = mock({ ...cand, itemsMalformed: true })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(m.itemPosts).toHaveLength(0)
  })

  it('the item POST itself failing → non-zero, no completion reported', async () => {
    const m = mock({ ...cand, failItemPost: true })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(m.itemPosts).toHaveLength(1) // attempted
    expect(s.out.join('\n')).not.toMatch(/enqueued/)
  })
})

// --- spec-invalid wire rows fail CLOSED (never dropped to "absent") ----------

describe('spec-invalid wire rows fail closed', () => {
  it('a malformed verdict-score row for an otherwise-scoreless trace → fail closed, NOT a fallback candidate', async () => {
    // t1 output PASS would be a fallback candidate IF it read as scoreless. The malformed
    // score (trace-kind, no subject.id) must fail closed rather than be dropped to absent.
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [
        {
          id: 'sc-bad',
          name: VERDICT_SCORE_NAME,
          value: 'pass',
          dataType: 'CATEGORICAL',
          subject: { kind: 'trace' },
        },
      ],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(s.er.join('\n')).toMatch(/BackfillDataError/)
    expect(s.out.join('\n')).not.toMatch(/covering-PASS candidate/)
  })

  it('a verdict-score row with a non-{pass,fail,unsure} value → fail closed', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [
        {
          id: 'sc-bad',
          name: VERDICT_SCORE_NAME,
          value: 'garbage',
          dataType: 'CATEGORICAL',
          subject: { kind: 'trace', id: 't1' },
        },
      ],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
  })

  it('a malformed existing queue-item → fail closed, NOT a duplicate POST', async () => {
    // The item has objectId===t1 but a malformed (non-string) objectType. Silently ignoring
    // it would let the enqueue duplicate a queued trace; it must fail closed instead.
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
      items: [{ id: 'e-bad', objectId: 't1', objectType: 42 }],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(m.itemPosts).toHaveLength(0)
    expect(s.er.join('\n')).toMatch(/objectType/)
  })

  it('a MISSING or UNKNOWN subject.kind on an otherwise-scoreless trace → fail closed, not admitted', async () => {
    // Both are corrupt rows: a KNOWN non-trace variant would be legitimately ignored, but a
    // missing/unknown kind that vanishes would let t1 (output PASS) sneak in via the fallback.
    for (const subject of [{ id: 't1' } as unknown, { kind: 'bogus', id: 't1' } as unknown]) {
      const m = mock({
        traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
        scores: [
          { id: 'sc-x', name: VERDICT_SCORE_NAME, value: 'pass', dataType: 'CATEGORICAL', subject },
        ],
      })
      vi.stubGlobal('fetch', m.fetchImpl)
      const s = sinks()
      const code = await main(['CON-042'], ENV, s.log, s.errlog)
      expect(code).toBe(1)
      expect(s.er.join('\n')).toMatch(/subject\.kind/)
      expect(s.out.join('\n')).not.toMatch(/covering-PASS candidate/)
      vi.restoreAllMocks()
    }
  })

  it('an existing item objectType of the wrong case ("trace") → fail closed, NOT a silent dup POST', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
      items: [{ id: 'e-bad', objectId: 't1', objectType: 'trace', status: 'PENDING' }],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(m.itemPosts).toHaveLength(0)
    expect(s.er.join('\n')).toMatch(/objectType/)
  })

  it('a matching item with an unknown status → fail closed, NOT a dup POST', async () => {
    const m = mock({
      traces: [trace('t1', ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore('t1', 'pass')],
      items: [{ id: 'e1', objectId: 't1', objectType: 'TRACE', status: 'ARCHIVED' }],
    })
    vi.stubGlobal('fetch', m.fetchImpl)
    const s = sinks()
    const code = await main(['CON-042', 't1'], ENV, s.log, s.errlog)
    expect(code).toBe(1)
    expect(m.itemPosts).toHaveLength(0)
    expect(s.er.join('\n')).toMatch(/status/)
  })
})

// --- v2 root observations: the subject is the trace id (PR2.c1) --------------

// Synthetic ids in the shapes of arc §2, S1: a historic string trace id, and a new
// 32-hex trace id whose root span id is a different 16-hex id.
const HIST_TRACE = 'judge-CON-042-sp0001-item0'
const HEX_TRACE = '3f2a9c0d4b6e1f7a8c5d2e9b0a1f4c7d'
const HEX_SPAN = '9b1c4e7a2d5f8a3c'
const HEX_GEN_SPAN = '0c4f7a1e9d2b5c8a'

describe('PR2.c1 (command level) — candidates keyed by traceId, never the observation id', () => {
  it('a historic root (observation id t-<traceId>) lists, links and joins by its string trace id', async () => {
    // The output says FAIL; only the PASS score bound to the trace id makes it a candidate,
    // so a join on the observation id would exclude it.
    const run = await runCommand(['CON-042'], {
      traces: [trace(HIST_TRACE, ['behavior:CON-042'], verdictOutput('fail', 'hc', 'hk'))],
      scores: [traceScore(HIST_TRACE, 'pass')],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(run.stdout).toMatch(/1 covering-PASS candidate/)
    expect(run.stdout).toContain(`\n  ${HIST_TRACE}\n`)
    expect(run.stdout).toContain(`${run.host}/project/proj-1/traces/${HIST_TRACE}`)
    expect(run.stdout).not.toContain(`t-${HIST_TRACE}`)
    expect(run.stdout).toContain('cite: hc')
    const [rootRead] = gets(run, OBSERVATIONS_V2)
    expect(rootReadTags(rootRead)).toEqual(['behavior:CON-042'])
    expect(gets(run, '/api/public/v3/scores')).toHaveLength(1)
    expect(writes(run)).toEqual([])
  })
  it('a new root (32-hex trace id, distinct span id) lists, links and joins by the hex trace id', async () => {
    const root = newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('fail'))
    const run = await runCommand(['CON-042'], {
      // The generation under the same trace carries the same trace tags; the root
      // filter keeps it from becoming a second row for the trace.
      traces: [root, childObs(root, HEX_GEN_SPAN)],
      scores: [traceScore(HEX_TRACE, 'pass')],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(run.stdout).toMatch(/1 covering-PASS candidate/)
    expect(run.stdout).toContain(`\n  ${HEX_TRACE}\n`)
    expect(run.stdout).toContain(`${run.host}/project/proj-1/traces/${HEX_TRACE}`)
    expect(run.stdout).not.toContain(HEX_SPAN)
    expect(run.stdout).not.toContain(HEX_GEN_SPAN)
    expect(rootReadTags(gets(run, OBSERVATIONS_V2)[0])).toEqual(['behavior:CON-042'])
    expect(writes(run)).toEqual([])
  })

  it('a round narrowed by runId: lists only roots carrying both tags', async () => {
    const run = await runCommand(['CON-042', '--round', RUN_ID], {
      traces: [
        newRoot(
          HEX_TRACE,
          HEX_SPAN,
          ['behavior:CON-042', `runId:${RUN_ID}`],
          verdictOutput('pass')
        ),
        trace(HIST_TRACE, ['behavior:CON-042', 'runId:20260716T151639Z'], verdictOutput('pass')),
        trace('judge-CON-042-sp0002-item0', ['behavior:CON-042'], verdictOutput('pass')),
      ],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(rootReadTags(gets(run, OBSERVATIONS_V2)[0])).toEqual([
      'behavior:CON-042',
      `runId:${RUN_ID}`,
    ])
    expect(run.stdout).toMatch(/1 covering-PASS candidate\(s\) for CON-042 \(runId:/)
    expect(run.stdout).toContain(`${run.host}/project/proj-1/traces/${HEX_TRACE}`)
    expect(run.stdout).not.toContain(HIST_TRACE)
    expect(run.stdout).not.toContain('sp0002')
    expect(writes(run)).toEqual([])
  })

  it('a round narrowed by gitSha: lists only roots carrying both tags', async () => {
    const run = await runCommand(['CON-042', '--round', 'abc1234'], {
      traces: [
        trace(HIST_TRACE, ['behavior:CON-042', 'gitSha:abc1234'], verdictOutput('pass')),
        newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042', 'gitSha:def5678'], verdictOutput('pass')),
      ],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(rootReadTags(gets(run, OBSERVATIONS_V2)[0])).toEqual([
      'behavior:CON-042',
      'gitSha:abc1234',
    ])
    expect(run.stdout).toMatch(/1 covering-PASS candidate\(s\) for CON-042 \(gitSha:abc1234\)/)
    expect(run.stdout).toContain(`${run.host}/project/proj-1/traces/${HIST_TRACE}`)
    expect(run.stdout).not.toContain(HEX_TRACE)
    expect(writes(run)).toEqual([])
  })

  it('a root carrying the behavior tag but not the round tag is excluded', async () => {
    const run = await runCommand(['CON-042', '--round', RUN_ID], {
      traces: [newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore(HEX_TRACE, 'pass')],
    })
    expect(run.code).toBe(3)
    expect(run.stderr).toMatch(/no traces tagged behavior:CON-042 \(runId:20260717T151639Z\)/)
    expect(run.stdout).toBe('')
    expect(writes(run)).toEqual([])
  })

  it('enqueue of a new-root candidate posts objectType TRACE with the hex traceId', async () => {
    const run = await runCommand(['CON-042', HEX_TRACE], {
      traces: [newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore(HEX_TRACE, 'pass')],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(run.stdout).toContain(`enqueued ${HEX_TRACE} into ${TRIAGE_QUEUE_NAME}`)
    // Membership is proven round-agnostic, then the existing items are read for dedup.
    expect(rootReadTags(gets(run, OBSERVATIONS_V2)[0])).toEqual(['behavior:CON-042'])
    expect(gets(run, `/api/public/annotation-queues/${triageQueue.id}/items`)).toHaveLength(1)
    expect(writes(run)).toEqual([
      {
        method: 'POST',
        path: `/api/public/annotation-queues/${triageQueue.id}/items`,
        body: { objectId: HEX_TRACE, objectType: 'TRACE' },
      },
    ])
  })

  it('enqueue by an observation id (new span id or historic t-<traceId>) is refused', async () => {
    for (const [root, obsId] of [
      [newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass')), HEX_SPAN],
      [trace(HIST_TRACE, ['behavior:CON-042'], verdictOutput('pass')), `t-${HIST_TRACE}`],
    ] as const) {
      const run = await runCommand(['CON-042', obsId], {
        traces: [root],
        scores: [traceScore(root.traceId, 'pass')],
      })
      expect(run.code, obsId).toBe(1)
      expect(run.stderr, obsId).toContain(`trace ${obsId} is not a covering-PASS candidate`)
      expect(run.stdout, obsId).toBe('')
      expect(writes(run), obsId).toEqual([])
    }
  })

  // Ruling R2: two fail-closed outcomes beyond PR2.c5, observed at the same level.
  it('a row outside the requested tags (a dropped filter) fails closed, lists nothing', async () => {
    const run = await runCommand(['CON-042', '--round', RUN_ID], {
      ignoreFilter: true,
      traces: [
        newRoot(
          HEX_TRACE,
          HEX_SPAN,
          ['behavior:CON-042', `runId:${RUN_ID}`],
          verdictOutput('pass')
        ),
        trace(HIST_TRACE, ['behavior:DSH-004'], verdictOutput('pass')),
      ],
    })
    expect(run.code).toBe(1)
    expect(run.stderr).toMatch(
      new RegExp(
        `BackfillDataError: root observation for trace ${HIST_TRACE} lacks the requested tags`
      )
    )
    expect(run.stdout).toBe('')
    expect(writes(run)).toEqual([])
  })

  it('two distinct root observations under one trace id fail closed, list nothing', async () => {
    const root = newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass'))
    const run = await runCommand(['CON-042'], {
      traces: [root, { ...root, id: '5e8b2d7f1a4c9e3b' }],
    })
    expect(run.code).toBe(1)
    expect(run.stderr).toMatch(
      new RegExp(`BackfillDataError: trace ${HEX_TRACE} has more than one root observation`)
    )
    expect(run.stdout).toBe('')
    expect(writes(run)).toEqual([])
  })

  it('enqueue refuses when two distinct root observations share the trace id', async () => {
    const root = newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass'))
    const run = await runCommand(['CON-042', HEX_TRACE], {
      traces: [root, { ...root, id: '5e8b2d7f1a4c9e3b' }],
      scores: [traceScore(HEX_TRACE, 'pass')],
    })
    expect(run.code).toBe(1)
    expect(run.stderr).toMatch(/BackfillDataError: .*more than one root observation/)
    expect(run.stdout).toBe('')
    expect(writes(run)).toEqual([])
  })
})

// --- output decoded on the client from v2's raw string (PR2.c2) --------------

describe('PR2.c2 (command level) — v2 output string decoded on the client; score-over-output and ambiguity unchanged', () => {
  const HEX_B = '7d1e4a9c3f6b2e8d5a0c7f1b4e9d2a6c'
  const HEX_B_SPAN = '2a5d8f1c4e7b0a3d'

  it('conflicting verdict scores on one trace → ambiguous, exits non-zero, lists nothing', async () => {
    const run = await runCommand(['CON-042'], {
      traces: [newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass'))],
      scores: [traceScore(HEX_TRACE, 'pass'), traceScore(HEX_TRACE, 'fail')],
    })
    expect(run.code).toBe(1)
    expect(run.stderr).toContain(`multiple verdict scores for trace(s) ${HEX_TRACE}`)
    expect(run.stderr).toMatch(/ambiguous/)
    expect(run.stdout).toBe('')
    expect(writes(run)).toEqual([])
  })

  it('a scoreless pass whose output string is JSON is a candidate with its cite and critique', async () => {
    const root = newRoot(
      HEX_TRACE,
      HEX_SPAN,
      ['behavior:CON-042'],
      verdictOutput('pass', 'jc', 'jk')
    )
    expect(typeof root.output).toBe('string')
    const run = await runCommand(['CON-042'], { traces: [root] })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(run.stdout).toMatch(/1 covering-PASS candidate/)
    expect(run.stdout).toContain(`\n  ${HEX_TRACE}\n`)
    expect(run.stdout).toContain('cite: jc')
    expect(run.stdout).toContain('critique: jk')
  })

  it('a malformed output string carries no verdict: scoreless → excluded; a PASS score still wins', async () => {
    const truncated = '{"itemIndex":0,"verdict":"pass","citation":"mc"'
    const run = await runCommand(['CON-042'], {
      traces: [
        { ...newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042']), output: truncated },
        { ...newRoot(HEX_B, HEX_B_SPAN, ['behavior:CON-042']), output: truncated },
      ],
      scores: [traceScore(HEX_B, 'pass')],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(run.stdout).toMatch(/1 covering-PASS candidate/)
    expect(run.stdout).toContain(`\n  ${HEX_B}\n`)
    expect(run.stdout).not.toContain(HEX_TRACE)
    expect(run.stdout).not.toContain('cite:')
  })

  it('a verdict score that contradicts the output wins, in both directions', async () => {
    const run = await runCommand(['CON-042'], {
      traces: [
        newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass')),
        newRoot(HEX_B, HEX_B_SPAN, ['behavior:CON-042'], verdictOutput('fail')),
      ],
      scores: [traceScore(HEX_TRACE, 'fail'), traceScore(HEX_B, 'pass')],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(run.stdout).toMatch(/1 covering-PASS candidate/)
    expect(run.stdout).toContain(`\n  ${HEX_B}\n`)
    expect(run.stdout).not.toContain(HEX_TRACE)
  })
})

// --- a root returned twice under the same (traceId, id) is one candidate (PR2.c4) ---

describe('PR2.c4 (command level) — duplicate root rows (an unmerged re-export) yield one candidate', () => {
  const listedOnce = (stdout: string, traceId: string): void => {
    expect(stdout.split('\n').filter(l => l === `  ${traceId}`)).toHaveLength(1)
  }

  it('a duplicate on the same page', async () => {
    const root = newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass'))
    const run = await runCommand(['CON-042'], {
      traces: [root, { ...root }],
      scores: [traceScore(HEX_TRACE, 'pass')],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(gets(run, OBSERVATIONS_V2)).toHaveLength(1)
    expect(run.stdout).toMatch(/1 covering-PASS candidate/)
    listedOnce(run.stdout, HEX_TRACE)
  })

  it('a duplicate across pages, including a page that holds only the duplicate', async () => {
    const root = newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass'))
    const other = trace(HIST_TRACE, ['behavior:CON-042'], verdictOutput('pass'))
    const run = await runCommand(['CON-042'], {
      traces: [root, { ...root }, other],
      tracesPerPage: 1,
      scores: [traceScore(HEX_TRACE, 'pass')],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    // Three cursor pages, each the same root read; the second follows the first's cursor.
    const reads = gets(run, OBSERVATIONS_V2)
    expect(reads.map(q => q.get('cursor'))).toEqual([null, '1', '2'])
    for (const q of reads) expect(rootReadTags(q)).toEqual(['behavior:CON-042'])
    expect(run.stdout).toMatch(/2 covering-PASS candidate/)
    listedOnce(run.stdout, HEX_TRACE)
    listedOnce(run.stdout, HIST_TRACE)
  })

  it('enqueue of a candidate whose root is duplicated posts it once', async () => {
    const root = newRoot(HEX_TRACE, HEX_SPAN, ['behavior:CON-042'], verdictOutput('pass'))
    const run = await runCommand(['CON-042', HEX_TRACE], {
      traces: [root, { ...root }],
      tracesPerPage: 1,
      scores: [traceScore(HEX_TRACE, 'pass')],
    })
    expect(run.stderr).toBe('')
    expect(run.code).toBe(0)
    expect(writes(run)).toEqual([
      {
        method: 'POST',
        path: `/api/public/annotation-queues/${triageQueue.id}/items`,
        body: { objectId: HEX_TRACE, objectType: 'TRACE' },
      },
    ])
  })
})
