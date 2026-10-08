import { afterAll, beforeAll, describe, expect, test } from 'bun:test'
import { assertCost } from './assert-cost'
import type { LangfuseConfig } from './http'

const cfg: LangfuseConfig = { host: 'https://fake-langfuse.test', publicKey: 'pk', secretKey: 'sk' }
const FROM = '2026-08-09T00:00:00.000Z'

type Row = Record<string, unknown>

// A v2 generation row (S3). `id`/`traceId` default to a unique pair per call.
let rowSeq = 0
function gen(model: string, totalCost: unknown, overrides: Row = {}): Row {
  rowSeq += 1
  const row: Row = { id: `span-${rowSeq}`, traceId: `trace-${rowSeq}`, model, ...overrides }
  if (totalCost !== undefined) row.totalCost = totalCost
  return row
}

function transportFor(rows: Row[]): typeof fetch {
  return async (input, init) => {
    const url = new URL(String(input))
    if (`${url.protocol}//${url.host}` !== cfg.host) {
      throw new Error(`unexpected host: ${url.href}`)
    }
    expect(init?.method ?? 'GET').toBe('GET')
    expect(url.pathname).toBe('/api/public/v2/observations')
    expect(url.searchParams.get('type')).toBe('GENERATION')
    expect(url.searchParams.get('fromStartTime')).toBe(FROM)
    expect(url.searchParams.get('fields')).toBe('core,model,usage')
    expect(url.searchParams.has('limit')).toBe(false)
    return new Response(JSON.stringify({ data: rows, meta: {} }), { status: 200 })
  }
}

describe('assertCost', () => {
  test('one or more GENERATION observations, all non-zero cost -> ok', async () => {
    const fetchFn = transportFor([gen('gpt-5.5', 0.0042), gen('gpt-5.6-luna', 0.0001)])
    const result = await assertCost(FROM, cfg, { fetchFn })
    expect(result.ok).toBe(true)
  })

  test('an observation with zero cost -> not ok, names the model string', async () => {
    const fetchFn = transportFor([gen('gpt-5.5', 0.0042), gen('gpt-5.6-terra', 0)])
    const result = await assertCost(FROM, cfg, { fetchFn })
    expect(result.ok).toBe(false)
    expect(result.message).toContain('gpt-5.6-terra')
  })

  test('an observation with absent cost -> not ok, names the model string', async () => {
    const fetchFn = transportFor([gen('gpt-5.4-mini', undefined)])
    const result = await assertCost(FROM, cfg, { fetchFn })
    expect(result.ok).toBe(false)
    expect(result.message).toContain('gpt-5.4-mini')
  })

  test('zero observations found -> not ok, distinct "nothing to assert" message', async () => {
    const fetchFn = transportFor([])
    const result = await assertCost(FROM, cfg, { fetchFn, retries: 0 })
    expect(result.ok).toBe(false)
    expect(result.message).toMatch(/no .*observations|nothing to assert/i)
  })
})

// Langfuse materializes exported observations asynchronously; only the
// zero-OBSERVATIONS case retries — a zero-COST observation is the signal itself.
describe('ingestion-lag retry', () => {
  function sequencedTransport(pages: Row[][]) {
    let call = 0
    const fetchFn: typeof fetch = async () => {
      const rows = pages[Math.min(call++, pages.length - 1)]
      return new Response(JSON.stringify({ data: rows, meta: {} }), { status: 200 })
    }
    return { fetchFn, callCount: () => call }
  }

  test('zero observations retries on the bounded schedule, then succeeds', async () => {
    const { fetchFn, callCount } = sequencedTransport([[], [], [gen('gpt-5.5', 0.01)]])
    const sleeps: number[] = []
    const result = await assertCost(FROM, cfg, {
      fetchFn,
      retries: 3,
      retryDelayMs: 5,
      sleep: async ms => void sleeps.push(ms),
    })
    expect(result.ok).toBe(true)
    expect(callCount()).toBe(3)
    expect(sleeps).toEqual([5, 5])
  })

  test('still zero after all retries -> nothing to assert', async () => {
    const { fetchFn, callCount } = sequencedTransport([[]])
    const sleeps: number[] = []
    const result = await assertCost(FROM, cfg, { fetchFn, retries: 2, retryDelayMs: 1, sleep: async ms => void sleeps.push(ms) })
    expect(result.ok).toBe(false)
    expect(result.message).toMatch(/nothing to assert/i)
    expect(callCount()).toBe(3)
    expect(sleeps).toHaveLength(2)
  })

  test('a zero-cost observation is reported immediately, never retried', async () => {
    const { fetchFn, callCount } = sequencedTransport([[gen('gpt-5.5', 0)]])
    const result = await assertCost(FROM, cfg, { fetchFn, retries: 3, retryDelayMs: 1, sleep: async () => {} })
    expect(result.ok).toBe(false)
    expect(callCount()).toBe(1)
  })
})

// The command level: `bun run assert-cost.ts <FROM>` is what `make qa-cost-assert`
// runs. The child gets only the fake server's host and dummy keys, never this
// process's environment.
describe('make qa-cost-assert command against a fake Langfuse server', () => {
  type Reply = { status?: number; body?: unknown; raw?: string }
  let respond: (url: URL) => Reply
  let requests: URL[]
  let server: ReturnType<typeof Bun.serve>

  beforeAll(() => {
    server = Bun.serve({
      port: 0,
      hostname: '127.0.0.1',
      fetch(req) {
        const url = new URL(req.url)
        requests.push(url)
        const reply = respond(url)
        return new Response(reply.raw ?? JSON.stringify(reply.body ?? {}), { status: reply.status ?? 200 })
      },
    })
  })
  afterAll(() => {
    void server.stop(true)
  })

  function serve(pages: Record<string, Reply>): void {
    requests = []
    respond = url => pages[url.searchParams.get('cursor') ?? ''] ?? { status: 404, body: { message: 'no such page' } }
  }

  async function run(args: string[] = [FROM], env: Record<string, string> = {}) {
    const proc = Bun.spawn(['bun', 'run', 'assert-cost.ts', ...args], {
      cwd: import.meta.dir,
      env: {
        PATH: process.env.PATH ?? '',
        LANGFUSE_HOST: `http://127.0.0.1:${server.port}`,
        LANGFUSE_PUBLIC_KEY: 'pk',
        LANGFUSE_SECRET_KEY: 'sk',
        QA_COST_ASSERT_RETRY_DELAY_MS: '0',
        ...env,
      },
      stdout: 'pipe',
      stderr: 'pipe',
    })
    const [stdout, stderr, code] = await Promise.all([
      new Response(proc.stdout).text(),
      new Response(proc.stderr).text(),
      proc.exited,
    ])
    return { stdout, stderr, code }
  }

  const page = (data: Row[], meta: Record<string, unknown> = {}): Reply => ({ body: { data, meta } })

  describe('PR1.c1: reads generations through v2', () => {
    test('every generation priced -> exit 0, counts them', async () => {
      serve({ '': page([gen('gpt-5.5', 0.002), gen('gpt-5.6-luna', 0.0001)]) })
      const out = await run()
      expect(out.code).toBe(0)
      expect(out.stdout).toContain('2 GENERATION observation(s)')
      expect(out.stdout).toContain('all priced')
      expect(requests).toHaveLength(1)
      const req = requests[0]
      expect(req.pathname).toBe('/api/public/v2/observations')
      expect(req.searchParams.get('type')).toBe('GENERATION')
      expect(req.searchParams.get('fromStartTime')).toBe(FROM)
      expect(req.searchParams.get('fields')).toBe('core,model,usage')
    })

    test('a generation with totalCost 0 -> exit 1, names that generation', async () => {
      serve({ '': page([gen('gpt-5.5', 0.002), gen('gpt-5.6-terra', 0, { id: 'span-zero', traceId: 'trace-zero' })]) })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stdout).toContain('gpt-5.6-terra')
      expect(out.stdout).toContain('trace-zero')
      expect(out.stdout).toContain('span-zero')
    })

    test('a generation with totalCost absent -> exit 1, names that generation', async () => {
      serve({ '': page([gen('gpt-5.4-mini', undefined, { id: 'span-absent', traceId: 'trace-absent' })]) })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stdout).toContain('gpt-5.4-mini')
      expect(out.stdout).toContain('span-absent')
    })

    test('a legacy costDetails total does not stand in for an absent totalCost', async () => {
      serve({ '': page([gen('gpt-5.5', undefined, { costDetails: { total: 0.5 }, calculatedTotalCost: 0.5 })]) })
      const out = await run()
      expect(out.code).toBe(1)
    })

    test('every failing generation is named, not only the first', async () => {
      serve({
        '': page([
          gen('model-a', 0, { id: 'span-a', traceId: 'trace-a' }),
          gen('gpt-5.5', 0.01),
          gen('model-b', undefined, { id: 'span-b', traceId: 'trace-b' }),
        ]),
      })
      const out = await run()
      expect(out.code).toBe(1)
      for (const needle of ['model-a', 'span-a', 'trace-a', 'model-b', 'span-b', 'trace-b']) {
        expect(out.stdout).toContain(needle)
      }
    })

    test('no generation found -> exit 1 "nothing to assert", after the bounded retries', async () => {
      serve({ '': page([]) })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stdout).toMatch(/nothing to assert/i)
      expect(requests).toHaveLength(4)
    })

    test('a request error -> exit 1 with the error on stderr', async () => {
      serve({ '': { status: 500, body: { message: 'boom' } } })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stderr).toContain('qa-cost-assert:')
      expect(out.stderr).toContain('500')
    })
  })

  describe('PR1.c2: v2 cursor pagination', () => {
    test('a single page with meta {} is the last page', async () => {
      serve({ '': page([gen('gpt-5.5', 0.002)], {}) })
      const out = await run()
      expect(out.code).toBe(0)
      expect(requests).toHaveLength(1)
      expect(requests[0].searchParams.has('cursor')).toBe(false)
    })

    test('three pages follow meta.cursor in order and never send a limit', async () => {
      serve({
        '': page([gen('gpt-5.5', 0.002)], { cursor: 'c1' }),
        c1: page([gen('gpt-5.5', 0.002)], { cursor: 'c2' }),
        c2: page([gen('gpt-5.5', 0.002)], {}),
      })
      const out = await run()
      expect(out.code).toBe(0)
      expect(out.stdout).toContain('3 GENERATION observation(s)')
      expect(requests.map(r => r.searchParams.get('cursor'))).toEqual([null, 'c1', 'c2'])
      for (const r of requests) {
        expect(r.searchParams.has('limit')).toBe(false)
        expect(r.searchParams.has('page')).toBe(false)
      }
    })

    for (const [label, cursor] of [
      ['a number', 7],
      ['an empty string', ''],
      ['null', null],
      ['an object', { next: 'c1' }],
    ] as const) {
      test(`a cursor that is ${label} fails the read`, async () => {
        serve({ '': page([gen('gpt-5.5', 0.002)], { cursor }) })
        const out = await run()
        expect(out.code).toBe(1)
        expect(out.stderr).toMatch(/cursor/)
        expect(requests).toHaveLength(1)
      })
    }

    test('a cursor equal to one already followed fails rather than loops', async () => {
      serve({
        '': page([gen('gpt-5.5', 0.002)], { cursor: 'c1' }),
        c1: page([gen('gpt-5.5', 0.002)], { cursor: 'c2' }),
        c2: page([gen('gpt-5.5', 0.002)], { cursor: 'c1' }),
      })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stderr).toMatch(/cursor/)
      expect(requests).toHaveLength(3)
    })

    test('a page that answers with its own cursor fails rather than loops', async () => {
      serve({
        '': page([gen('gpt-5.5', 0.002)], { cursor: 'c1' }),
        c1: page([gen('gpt-5.5', 0.002)], { cursor: 'c1' }),
      })
      const out = await run()
      expect(out.code).toBe(1)
      expect(requests).toHaveLength(2)
    })

    test('a page missing data fails the read', async () => {
      serve({ '': { body: { meta: {} } } })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stderr).toMatch(/data/)
    })

    test('a page whose data is not an array fails the read', async () => {
      serve({ '': { body: { data: { rows: [] }, meta: {} } } })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stderr).toMatch(/data/)
    })

    test('a page missing meta fails the read rather than ending it', async () => {
      serve({ '': { body: { data: [gen('gpt-5.5', 0.002)] } } })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stderr).toContain('no meta object')
    })

    test('a later page missing data fails the read, not just the first', async () => {
      serve({
        '': page([gen('gpt-5.5', 0.002)], { cursor: 'c1' }),
        c1: { body: { meta: {} } },
      })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stderr).toMatch(/data/)
    })
  })

  describe('PR1.c3: duplicates count once by (traceId, id)', () => {
    test('a duplicate on the same page is counted once', async () => {
      const g = gen('gpt-5.5', 0.002)
      serve({ '': page([g, { ...g }]) })
      const out = await run()
      expect(out.code).toBe(0)
      expect(out.stdout).toContain('1 GENERATION observation(s)')
    })

    test('a duplicate across pages is counted once', async () => {
      const g = gen('gpt-5.5', 0.002)
      serve({ '': page([g], { cursor: 'c1' }), c1: page([{ ...g }, gen('gpt-5.5', 0.002)], {}) })
      const out = await run()
      expect(out.code).toBe(0)
      expect(out.stdout).toContain('2 GENERATION observation(s)')
    })

    test('one span id under two trace ids is two generations', async () => {
      serve({
        '': page([
          gen('gpt-5.5', 0.002, { id: 'shared-span', traceId: 'trace-one' }),
          gen('gpt-5.5', 0.002, { id: 'shared-span', traceId: 'trace-two' }),
        ]),
      })
      const out = await run()
      expect(out.code).toBe(0)
      expect(out.stdout).toContain('2 GENERATION observation(s)')
    })

    test('a duplicated unpriced generation is named once', async () => {
      const g = gen('model-dup', 0, { id: 'span-dup', traceId: 'trace-dup' })
      serve({ '': page([g, { ...g }]) })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stdout.match(/span-dup/g)).toHaveLength(1)
    })

    test('a row without an id and traceId fails the read rather than merging with others', async () => {
      serve({ '': page([{ model: 'gpt-5.5', totalCost: 0.002 }, { model: 'gpt-5.5', totalCost: 0 }]) })
      const out = await run()
      expect(out.code).toBe(1)
      expect(out.stderr).toMatch(/traceId|id/)
    })
  })

  describe('PR1.c4: exit codes the nightly reads stay as they are', () => {
    test('missing Langfuse env -> exit 2', async () => {
      serve({ '': page([]) })
      const out = await run([FROM], { LANGFUSE_HOST: '' })
      expect(out.code).toBe(2)
      expect(out.stderr).toContain('LANGFUSE_HOST')
      expect(requests).toHaveLength(0)
    })

    test('missing FROM -> exit 2 with usage', async () => {
      serve({ '': page([]) })
      const out = await run([])
      expect(out.code).toBe(2)
      expect(out.stderr).toContain('usage')
      expect(requests).toHaveLength(0)
    })
  })
})
