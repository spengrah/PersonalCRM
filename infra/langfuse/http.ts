// Dependency-free Langfuse HTTP helper for infra/langfuse tooling.
//
// This deliberately duplicates the ~40-line Basic-auth fetch helper that already
// exists in frontend/tests/tours/judge/export/langfuse.ts. Importing across
// infra/ -> frontend/tests/tours/judge/ would recreate the exact coupling this
// factoring removes — infra/langfuse/ is a self-contained unit with zero imports
// from the judge tree. See the spec's "Architectural direction" section.

export interface LangfuseConfig {
  host: string
  publicKey: string
  secretKey: string
}

// Resolve config from env. Returns undefined when Langfuse is not configured.
export function configFromEnv(
  env: Record<string, string | undefined> = process.env
): LangfuseConfig | undefined {
  const host = env.LANGFUSE_HOST
  const publicKey = env.LANGFUSE_PUBLIC_KEY
  const secretKey = env.LANGFUSE_SECRET_KEY
  if (!host || !publicKey || !secretKey) return undefined
  return { host, publicKey, secretKey }
}

function authHeader(cfg: LangfuseConfig): string {
  return 'Basic ' + Buffer.from(`${cfg.publicKey}:${cfg.secretKey}`).toString('base64')
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly body: string,
    readonly method: string,
    readonly path: string
  ) {
    super(`${method} ${path} -> ${status} ${body.slice(0, 200)}`)
    this.name = 'ApiError'
  }
}

// The fetch signature, injectable so tests can supply a fake transport instead of
// a real network call.
export type FetchFn = typeof fetch

export const REQUEST_TIMEOUT_MS = 30_000

// A single Langfuse public-API call. Always builds the request URL as
// `${cfg.host}${path}` and nothing else — apply.ts and assert-cost.ts rely on that
// fact to never reach any host but Langfuse's.
export async function api(
  cfg: LangfuseConfig,
  method: string,
  path: string,
  body?: unknown,
  fetchFn: FetchFn = fetch
): Promise<Record<string, unknown>> {
  const res = await fetchFn(`${cfg.host}${path}`, {
    method,
    headers: { Authorization: authHeader(cfg), 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
    // A hung connection must fail loudly, not stall the caller: the nightly
    // round's fail-open handling only fires if apply/assert-cost actually exit.
    signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
  })
  const text = await res.text()
  if (!res.ok) throw new ApiError(res.status, text, method, path)
  return text ? (JSON.parse(text) as Record<string, unknown>) : {}
}

// Walk a page-style paginated Langfuse list endpoint to completion (page+limit
// request params; meta `{page, limit, totalItems, totalPages}`). Merges the
// pagination params onto whatever query the caller already put on `path`.
export async function apiGetAllPages(
  cfg: LangfuseConfig,
  path: string,
  fetchFn: FetchFn = fetch
): Promise<Array<Record<string, unknown>>> {
  const items: Array<Record<string, unknown>> = []
  const limit = 100
  let page = 1
  for (;;) {
    const sep = path.includes('?') ? '&' : '?'
    const res = await api(cfg, 'GET', `${path}${sep}page=${page}&limit=${limit}`, undefined, fetchFn)
    // A 200 with a malformed pagination envelope must fail loudly: silently
    // treating a partial page as the complete list would let apply act on
    // partial state (e.g. re-create definitions that exist on omitted pages).
    if (!Array.isArray(res.data)) {
      throw new Error(`GET ${path}: page ${page} response has no data array`)
    }
    const data = res.data as Array<Record<string, unknown>>
    const meta = res.meta as Record<string, unknown> | undefined
    const totalPages = typeof meta?.totalPages === 'number' ? meta.totalPages : undefined
    if (totalPages === undefined) {
      throw new Error(`GET ${path}: page ${page} response has no meta.totalPages`)
    }
    if (typeof meta?.page === 'number' && meta.page !== page) {
      throw new Error(`GET ${path}: requested page ${page} but response echoes page ${meta.page}`)
    }
    if (data.length === 0 && page < totalPages) {
      throw new Error(`GET ${path}: page ${page} of ${totalPages} is unexpectedly empty`)
    }
    items.push(...data)
    if (page >= totalPages) break
    page += 1
  }
  return items
}
