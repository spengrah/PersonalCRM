import { spawnSync } from 'child_process'
import { mkdirSync } from 'fs'
import path from 'path'
import { main as report } from './report/render'

async function main(): Promise<void> {
  const repo = path.resolve(import.meta.dirname ?? __dirname, '../../../..')
  const runsRoot = path.join(repo, 'frontend/tests/tours/.runs')
  const runId = new Date()
    .toISOString()
    .replace(/[-:]/g, '')
    .replace(/\.\d{3}/, '')
  mkdirSync(runsRoot, { recursive: true })
  const runDir = path.join(runsRoot, runId)
  // Exclusive creation prevents a same-second invocation from grading an older run.
  mkdirSync(runDir)
  const tours = spawnSync('bash', [path.join(repo, 'scripts/run-tours.sh')], {
    cwd: repo,
    env: { ...process.env, TOURS_RUN_ID: runId },
    stdio: 'inherit',
  })
  if (tours.error) throw tours.error
  if (tours.status !== 0) throw new Error(`tours failed (${tours.status ?? tours.signal})`)
  await report([runDir])
}

if (import.meta.main) {
  main().catch(error => {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  })
}
