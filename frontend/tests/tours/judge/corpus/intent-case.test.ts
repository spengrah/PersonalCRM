// Intent-case schema rules (mirror of the Case rules): doctored requires a
// mutation, clean must not carry one; committed intent-cases load and their
// capture refs + intent ids resolve.

import * as path from 'path'
import { describe, expect, it } from 'vitest'
import { INTENT_CATALOG } from '../intent-catalog'
import { loadCorpus } from './load'
import { parseIntentCase } from './schema'

const CORPUS = path.join(import.meta.dirname ?? __dirname)

const clean = {
  id: 'x-clean',
  intent_id: 'DSH-010',
  source: 'clean',
  captures: ['dashboard/a.json'],
  expected_hypothesis: 'pass',
}

describe('parseIntentCase', () => {
  it('accepts a clean case and a doctored case with a mutation', () => {
    expect(parseIntentCase(clean).id).toBe('x-clean')
    expect(
      parseIntentCase({
        ...clean,
        id: 'x-doc',
        source: 'doctored',
        mutation: { op: 'blank_dialog' },
        expected_hypothesis: 'fail',
      }).source
    ).toBe('doctored')
  })

  it('rejects doctored-without-mutation and clean-with-mutation', () => {
    expect(() => parseIntentCase({ ...clean, source: 'doctored' })).toThrow(/requires a mutation/)
    expect(() => parseIntentCase({ ...clean, mutation: { op: 'blank_dialog' } })).toThrow(
      /must not carry a mutation/
    )
  })
})

describe('committed intent-cases', () => {
  const corpus = loadCorpus(CORPUS)

  it('load, reference known intents, and resolve their captures', () => {
    expect(corpus.intentCases.length).toBeGreaterThanOrEqual(2)
    for (const ic of corpus.intentCases) {
      expect(INTENT_CATALOG[ic.intent_id], `${ic.id} intent`).toBeDefined()
      const captures = corpus.capturesFor(ic)
      expect(captures.length).toBe(ic.captures.length)
    }
  })
})
