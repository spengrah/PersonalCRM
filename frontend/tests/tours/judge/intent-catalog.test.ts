// The INTENT_CATALOG ↔ SSOT sync guard: parses the three toured domains' spec
// YAML and asserts the transcription matches — intent ids/titles/statements/
// status verbatim, and servedBy equal to the corpus-wide inversion of the
// `serves:` edges. Catalog drift fails HERE (offline), never in a live run.
//
// The catalog transcribes the NON-RETIRED intent set: a retired intent is an
// SSOT tombstone (its row stays for the ID to never be reused) but is absent
// from the judged catalog, so it must never re-enroll.

import * as fs from 'fs'
import * as path from 'path'
import { parse } from 'yaml'
import { describe, expect, it } from 'vitest'
import { INTENT_CATALOG } from './intent-catalog'

const SPEC_DIR = path.join(import.meta.dirname ?? __dirname, '..', '..', '..', '..', 'spec')

interface YamlBehavior {
  id: string
  title: string
  type: string
  status: string
  statement?: string
  serves?: string | string[]
}

// Genuinely corpus-wide: every spec/*.yaml, matching the linter's resolution
// scope — a cross-domain serves edge or an intent minted in a non-toured
// domain lands in the inversion (and fails the sync assertions) instead of
// silently under-binding evidence.
function loadBehaviors(): YamlBehavior[] {
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

describe('intent-catalog ↔ spec YAML sync', () => {
  const behaviors = loadBehaviors()
  const yamlIntents = behaviors.filter(b => b.type === 'intent' && b.status !== 'retired')

  it('transcribes exactly the intent set of the whole corpus', () => {
    expect(Object.keys(INTENT_CATALOG).sort()).toEqual(yamlIntents.map(b => b.id).sort())
  })

  it('transcribes title/statement/status verbatim', () => {
    for (const b of yamlIntents) {
      const spec = INTENT_CATALOG[b.id]
      expect(spec, `catalog missing ${b.id}`).toBeDefined()
      expect(spec.title, `${b.id} title`).toBe(b.title)
      expect(spec.statement, `${b.id} statement`).toBe(b.statement)
      expect(spec.status, `${b.id} status`).toBe(b.status)
    }
  })

  it('servedBy is the inversion of the YAML serves edges', () => {
    const inverted: Record<string, string[]> = {}
    for (const b of behaviors) {
      const serves = b.serves === undefined ? [] : Array.isArray(b.serves) ? b.serves : [b.serves]
      for (const target of serves) {
        ;(inverted[target] ??= []).push(b.id)
      }
    }
    for (const id of Object.keys(INTENT_CATALOG)) {
      expect(INTENT_CATALOG[id].servedBy.slice().sort(), `${id} servedBy`).toEqual(
        (inverted[id] ?? []).sort()
      )
    }
    // No serves edge points at a non-catalog target within the toured domains.
    for (const target of Object.keys(inverted)) {
      expect(INTENT_CATALOG[target], `serves target ${target} missing from catalog`).toBeDefined()
    }
  })

  it('every servedBy entry names a behavior that exists in the YAML', () => {
    const ids = new Set(behaviors.map(b => b.id))
    for (const spec of Object.values(INTENT_CATALOG)) {
      for (const b of spec.servedBy) expect(ids.has(b), `${spec.id} servedBy ${b}`).toBe(true)
    }
  })
})
