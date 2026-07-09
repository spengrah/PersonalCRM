// Corpus loader — bun runtime only (Bun.YAML + fs). NOT imported by any vitest
// test (the pure schema/doctor/eval logic is tested over already-parsed
// objects), so its use of Bun.YAML never runs under node/vitest.

import * as fs from 'fs'
import * as path from 'path'
import type { Capture } from '../../support/types'
import { parseCase, type Case } from './schema'

export function loadCapture(capturesRoot: string, ref: string): Capture {
  return JSON.parse(fs.readFileSync(path.join(capturesRoot, ref), 'utf8')) as Capture
}

export function loadCaseFile(file: string): Case {
  return parseCase(Bun.YAML.parse(fs.readFileSync(file, 'utf8')))
}

export interface LoadedCorpus {
  cases: Case[]
  byId: Map<string, Case>
  capturesRoot: string
  capturesFor(c: Case): Capture[]
}

export function loadCorpus(corpusRoot: string): LoadedCorpus {
  const casesDir = path.join(corpusRoot, 'cases')
  const capturesRoot = path.join(corpusRoot, 'captures')
  const cases: Case[] = []
  if (fs.existsSync(casesDir)) {
    for (const f of fs.readdirSync(casesDir).sort()) {
      if (!/\.ya?ml$/.test(f)) continue
      cases.push(loadCaseFile(path.join(casesDir, f)))
    }
  }
  return {
    cases,
    byId: new Map(cases.map(c => [c.id, c])),
    capturesRoot,
    capturesFor: (c: Case): Capture[] => c.captures.map(r => loadCapture(capturesRoot, r)),
  }
}
