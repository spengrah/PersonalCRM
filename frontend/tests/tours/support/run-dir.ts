// Run-directory resolution + the runId handoff between globalSetup (main
// process) and capture() (worker process). globalSetup writes a CURRENT_RUN_ID
// marker; capture() reads it (preferring TOURS_RUN_ID when the wrapper exported
// one). Both anchor the .runs tree relative to this file, so the resolution is
// independent of the invocation cwd.

import * as fs from 'fs'
import * as path from 'path'

// support/ → tests/tours/. `typeof __dirname` is safe even when undeclared.
const supportDir =
  typeof __dirname !== 'undefined' ? __dirname : path.resolve(process.cwd(), 'tests/tours/support')

export const RUNS_ROOT = path.resolve(supportDir, '..', '.runs')
const MARKER = path.join(RUNS_ROOT, 'CURRENT_RUN_ID')

// A filesystem-safe wall-clock run id (the run's wall timestamp).
export function generateRunId(): string {
  return new Date().toISOString().replace(/[:.]/g, '-')
}

export function setCurrentRunId(runId: string): void {
  fs.mkdirSync(RUNS_ROOT, { recursive: true })
  fs.writeFileSync(MARKER, runId, 'utf8')
}

export function getCurrentRunId(): string {
  const fromEnv = process.env.TOURS_RUN_ID
  if (fromEnv) return fromEnv
  try {
    return fs.readFileSync(MARKER, 'utf8').trim()
  } catch {
    throw new Error('tours: no current run id — globalSetup must run before capture()')
  }
}

export function runDir(runId: string): string {
  return path.join(RUNS_ROOT, runId)
}

export function capturesDir(runId: string, tour: string): string {
  return path.join(runDir(runId), 'captures', tour)
}

export function manifestPath(runId: string): string {
  return path.join(runDir(runId), 'manifest.json')
}
