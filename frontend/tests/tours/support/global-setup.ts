// Tours globalSetup: validate required env, establish the run dir, and write
// the run manifest. Runs once in the main process before any worker; the runId
// is handed to capture() via run-dir's marker.

import * as fs from 'fs'
import type { FullConfig } from '@playwright/test'
import { buildManifest } from './manifest'
import { generateRunId, manifestPath, runDir, setCurrentRunId } from './run-dir'

export default async function globalSetup(_config: FullConfig): Promise<void> {
  const required = ['TOURS_BASE_URL', 'TOURS_API_KEY']
  const missing = required.filter(k => !process.env[k])
  if (missing.length > 0) {
    throw new Error(
      `tours: missing required env: ${missing.join(', ')} — run via scripts/run-tours.sh (make tours)`
    )
  }

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
