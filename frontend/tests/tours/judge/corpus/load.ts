// Corpus loader. Cases + labels are committed as JSON (parsed with the portable
// JSON.parse — no Bun.YAML dependency), so the loader runs identically under
// node/vitest AND bun; the loader is exercised end-to-end by a vitest.

import * as fs from 'fs'
import * as path from 'path'
import type { Capture } from '../../support/types'
import { parseCase, parseIntentCase, type Case, type IntentCase } from './schema'

export function loadCapture(capturesRoot: string, ref: string): Capture {
  return JSON.parse(fs.readFileSync(path.join(capturesRoot, ref), 'utf8')) as Capture
}

export function loadCaseFile(file: string): Case {
  return parseCase(JSON.parse(fs.readFileSync(file, 'utf8')))
}

export interface LoadedCorpus {
  cases: Case[]
  byId: Map<string, Case>
  // Intent-pass hypothesis cases (corpus/intent-cases/) — advisory --judge only.
  intentCases: IntentCase[]
  capturesRoot: string
  capturesFor(c: Case | IntentCase): Capture[]
}

export function loadCorpus(corpusRoot: string): LoadedCorpus {
  const casesDir = path.join(corpusRoot, 'cases')
  const intentCasesDir = path.join(corpusRoot, 'intent-cases')
  const capturesRoot = path.join(corpusRoot, 'captures')
  const cases: Case[] = []
  if (fs.existsSync(casesDir)) {
    for (const f of fs.readdirSync(casesDir).sort()) {
      if (!f.endsWith('.json')) continue
      cases.push(loadCaseFile(path.join(casesDir, f)))
    }
  }
  const intentCases: IntentCase[] = []
  if (fs.existsSync(intentCasesDir)) {
    for (const f of fs.readdirSync(intentCasesDir).sort()) {
      if (!f.endsWith('.json')) continue
      intentCases.push(
        parseIntentCase(JSON.parse(fs.readFileSync(path.join(intentCasesDir, f), 'utf8')))
      )
    }
  }
  return {
    cases,
    byId: new Map(cases.map(c => [c.id, c])),
    intentCases,
    capturesRoot,
    capturesFor: (c: Case | IntentCase): Capture[] =>
      c.captures.map(r => loadCapture(capturesRoot, r)),
  }
}
