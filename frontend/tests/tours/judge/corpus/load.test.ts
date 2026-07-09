import { describe, it, expect } from 'vitest'
import * as path from 'path'
import { loadCaseFile, loadCorpus } from './load'

// This test runs under vitest/NODE (no Bun) — so it proves the loader parses
// the committed corpus WITHOUT Bun.YAML. A regression to a bun-only parser
// would crash here in the node test image.
describe('loadCorpus (portable JSON — no Bun.YAML)', () => {
  const corpusRoot = __dirname

  it('loads every committed corpus case end-to-end', () => {
    const corpus = loadCorpus(corpusRoot)
    expect(corpus.cases.length).toBeGreaterThanOrEqual(12)
    // Both clean and doctored cases parse.
    expect(corpus.cases.some(c => c.source === 'clean')).toBe(true)
    expect(corpus.cases.some(c => c.source === 'doctored')).toBe(true)
  })

  it('resolves each case to real capture fixtures', () => {
    const corpus = loadCorpus(corpusRoot)
    const clean = corpus.byId.get('CON-041-clean')
    expect(clean).toBeDefined()
    const captures = corpus.capturesFor(clean!)
    expect(captures.length).toBe(2)
    expect(captures[0].captureFormatVersion).toBe(1)
    expect(captures[0].behaviors).toContain('CON-041')
  })

  it('loadCaseFile parses a single committed case', () => {
    const c = loadCaseFile(path.join(corpusRoot, 'cases', 'CON-042-doctored-nowarn.json'))
    expect(c.source).toBe('doctored')
    expect(c.doctor?.mutation.op).toBe('blank_dialog')
  })
})
