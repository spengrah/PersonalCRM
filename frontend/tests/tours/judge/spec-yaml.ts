// Reads the behavior SSOT (every spec/*.yaml) for the catalog ↔ YAML sync tests.

import * as fs from 'fs'
import * as path from 'path'
import { parse } from 'yaml'

const SPEC_DIR = path.join(import.meta.dirname ?? __dirname, '..', '..', '..', '..', 'spec')

export interface YamlBehavior {
  id: string
  title: string
  type: string
  status: string
  statement?: string
  serves?: string | string[]
  given?: string
  when?: string
  then?: { key: string; text: string }[]
}

// Genuinely corpus-wide: every spec/*.yaml, matching the linter's resolution
// scope — a cross-domain serves edge or an intent minted in a non-toured
// domain lands in the inversion (and fails the sync assertions) instead of
// silently under-binding evidence.
export function loadSpecBehaviors(): YamlBehavior[] {
  return fs
    .readdirSync(SPEC_DIR)
    .filter(f => f.endsWith('.yaml'))
    .flatMap(f => {
      const doc = parse(fs.readFileSync(path.join(SPEC_DIR, f), 'utf8')) as {
        behaviors: YamlBehavior[]
      }
      return doc.behaviors
    })
}
