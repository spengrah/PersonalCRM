// Fail-closed production guard for the tours target, extracted from
// global-setup.ts so it is unit-testable with an injected fetch (no live
// network). The tours issue real mutations (delete, merge, mark-contacted)
// against the Playwright target and write captures, so we refuse to run unless
// that target self-reports a NON-production environment.
//
// Pure aside from the injected fetch — no Playwright import.

// A target environment is safe to tour only if it is present AND not a
// production alias. Empty / unknown / missing is treated as production
// (fail-closed).
//
// When-you-change-X-check-Y: this deny-set MUST stay in lockstep with the
// backend's own production gate, config.IsProductionCRMEnv (Go). The two are
// the same policy expressed on two sides of the wire; if the backend adds a new
// production alias, add it here too or a destructive tour could run against it.
export const PROD_OR_UNKNOWN = new Set(['', 'production', 'prod', 'unknown'])

export interface ProdGuardOptions {
  /**
   * The API base to probe. Defaults to TOURS_API_URL, then TOURS_BASE_URL.
   * Resolution order matters: the guard must check the API base (where the
   * backend's `environment` is truthful), NOT the frontend origin — a
   * split-origin run (TOURS_API_URL !== TOURS_BASE_URL) points them at
   * different hosts and only the API host reports CRM_ENV.
   */
  apiBase?: string
  apiKey?: string
  /** Injected for tests; defaults to the global fetch. */
  fetchImpl?: typeof fetch
}

export async function assertNonProductionTarget(opts: ProdGuardOptions = {}): Promise<void> {
  const apiBase = (
    opts.apiBase ??
    process.env.TOURS_API_URL ??
    process.env.TOURS_BASE_URL ??
    ''
  ).replace(/\/$/, '')
  const apiKey = opts.apiKey ?? process.env.TOURS_API_KEY ?? ''
  const fetchImpl = opts.fetchImpl ?? fetch
  const url = `${apiBase}/api/v1/system/time`

  let environment: string | undefined
  try {
    const resp = await fetchImpl(url, { headers: { 'X-API-Key': apiKey } })
    if (!resp.ok) throw new Error(`GET /api/v1/system/time returned ${resp.status}`)
    const body = (await resp.json()) as { data?: { environment?: string } }
    environment = body?.data?.environment
  } catch (err) {
    throw new Error(
      `tours: REFUSING — could not verify the target environment via ${url}: ` +
        `${err instanceof Error ? err.message : String(err)}. ` +
        'Tours mutate data and must run only against a verified non-production target.'
    )
  }

  if (PROD_OR_UNKNOWN.has((environment ?? '').trim().toLowerCase())) {
    throw new Error(
      `tours: REFUSING — target reports environment='${environment ?? ''}' ` +
        '(production / empty / unknown). Tours issue real mutations (delete/merge/mark-contacted) ' +
        'and write captures, so they run ONLY against a non-production target.'
    )
  }
}
