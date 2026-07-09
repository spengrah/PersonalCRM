// Tours globalSetup: validate required env, establish the run dir, and write
// the run manifest. Runs once in the main process before any worker; the runId
// is handed to capture() via run-dir's marker.

import * as fs from 'fs'
import type { FullConfig } from '@playwright/test'
import { buildManifest } from './manifest'
import { generateRunId, manifestPath, runDir, setCurrentRunId } from './run-dir'

// A target environment is safe to tour only if it is present AND not a
// production alias. Empty / unknown / missing is treated as production
// (fail-closed, mirroring the backend's own production gate).
const PROD_OR_UNKNOWN = new Set(['', 'production', 'prod', 'unknown'])

// Fail-closed production guard: the tours issue real mutations (delete, merge,
// mark-contacted) against the Playwright target and write captures, so refuse to
// run unless that target self-reports a non-production environment. Uses the
// same GET /api/v1/system/time the captures rely on; its `environment` is the
// backend's CRM_ENV. This guards the actual test target, independent of the
// (decoupled) staging-reset host.
async function assertNonProductionTarget(): Promise<void> {
  const apiBase = (process.env.TOURS_API_URL || process.env.TOURS_BASE_URL || '').replace(/\/$/, '')
  const apiKey = process.env.TOURS_API_KEY ?? ''
  const url = `${apiBase}/api/v1/system/time`
  let environment: string | undefined
  try {
    const resp = await fetch(url, { headers: { 'X-API-Key': apiKey } })
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

export default async function globalSetup(_config: FullConfig): Promise<void> {
  const required = ['TOURS_BASE_URL', 'TOURS_API_KEY']
  const missing = required.filter(k => !process.env[k])
  if (missing.length > 0) {
    throw new Error(
      `tours: missing required env: ${missing.join(', ')} — run via scripts/run-tours.sh (make tours)`
    )
  }

  // Refuse to touch a production (or unverifiable) target before anything runs.
  await assertNonProductionTarget()

  const runId = process.env.TOURS_RUN_ID || generateRunId()
  process.env.TOURS_RUN_ID = runId

  fs.mkdirSync(runDir(runId), { recursive: true })
  setCurrentRunId(runId)

  const manifest = buildManifest({
    runId,
    gitSha: process.env.TOURS_GIT_SHA,
    stagingImageDigest: process.env.TOURS_IMAGE_DIGEST,
    seedProfile: process.env.TOURS_SEED_PROFILE,
    baseUrl: process.env.TOURS_BASE_URL,
  })
  fs.writeFileSync(manifestPath(runId), `${JSON.stringify(manifest, null, 2)}\n`, 'utf8')
}
