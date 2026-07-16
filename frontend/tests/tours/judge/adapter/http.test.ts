// The http side of the both-adapters test. This adapter was METRICS-ONLY; PR2
// makes it emit content (prompt/response/scenario/graded_evidence) symmetrically
// with codex-exec — but text-only, so it attaches NO screenshots. And, like
// codex, its qa.item_verdicts must EQUAL the NORMALIZED array it RETURNS across
// every path (built after normalization, not from a pre-normalization parse).

import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { describe, expect, it } from 'vitest'
import { parseSpanFile } from '../export/langfuse'
import type { GradedEvidenceEntry } from '../label-trace'
import { makeHttpJudge } from './http'
import type { JudgeInput, PerItemVerdict } from './types'

const contentInput: JudgeInput = {
  behaviorId: 'CON-042',
  behaviorTitle: 'delete confirmation',
  given: 'a contact exists',
  when: 'the user deletes it',
  then: ['warns cannot be undone', 'closes', 'removes the row'],
  items: [
    { itemIndex: 0, thenText: 'warns cannot be undone' },
    { itemIndex: 2, thenText: 'removes the row' },
  ],
  evidence: {},
  captureSections: [
    { captureFile: '001.json', note: 'dialog', evidence: { url: 'http://x/1' } },
    { captureFile: '002.json', note: 'gone', evidence: { url: 'http://x/2' } },
  ],
  // Present, but the http adapter is text-only and must NOT attach them.
  images: ['/runs/001.png', '/runs/002.png'],
}

// A fetch stub returning a chat/completions body with the given content (or
// throwing / a non-ok status to exercise the error path).
function fetchReturning(
  content: string | undefined,
  opts: { ok?: boolean; throws?: boolean } = {}
): typeof fetch {
  return (async () => {
    if (opts.throws) throw new Error('network down')
    return {
      ok: opts.ok ?? true,
      status: opts.ok === false ? 500 : 200,
      json: async () => ({
        choices: content === undefined ? [] : [{ message: { content } }],
        usage: { prompt_tokens: 50, completion_tokens: 10 },
      }),
    } as Response
  }) as unknown as typeof fetch
}

async function spanFor(
  fetchImpl: typeof fetch
): Promise<{ span: ReturnType<typeof parseSpanFile>[number]; verdicts: PerItemVerdict[] }> {
  const tracePath = path.join(
    os.tmpdir(),
    `qa-http-span-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2)}.jsonl`
  )
  try {
    const verdicts = await makeHttpJudge({ url: 'http://judge', fetchImpl, tracePath })(
      contentInput
    )
    const [span] = parseSpanFile(fs.readFileSync(tracePath, 'utf8'))
    return { span, verdicts }
  } finally {
    fs.rmSync(tracePath, { force: true })
  }
}

const verdictsBody = (verdicts: unknown[]): string => JSON.stringify({ verdicts })

describe('makeHttpJudge — now emits content, text-only (no screenshots)', () => {
  it('carries scenario + graded_evidence + prompt + completion, but attaches NO screenshots', async () => {
    const { span } = await spanFor(
      fetchReturning(
        verdictsBody([
          { item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'k' },
          { item_index: 2, verdict: 'pass', citation: 'row', critique: 'k' },
        ])
      )
    )
    expect((span.attributes['qa.scenario'] as { kind: string }).kind).toBe('behavior')
    const graded = span.attributes['qa.graded_evidence'] as GradedEvidenceEntry[]
    expect(graded.map(g => g.captureFile)).toEqual(['001.json', '002.json'])
    // Text-only: NO screenshot on any entry, and no qa.screenshots attribute.
    expect(graded.every(g => g.screenshot === undefined)).toBe(true)
    expect('qa.screenshots' in span.attributes).toBe(false)
    expect(typeof span.attributes['gen_ai.prompt']).toBe('string')
    expect(typeof span.attributes['gen_ai.completion']).toBe('string')
  })

  it('(a) success: item_verdicts equals the returned verdicts', async () => {
    const { span, verdicts } = await spanFor(
      fetchReturning(
        verdictsBody([
          { item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'k' },
          { item_index: 2, verdict: 'pass', citation: 'row', critique: 'k' },
        ])
      )
    )
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(verdicts.map(v => v.verdict)).toEqual(['fail', 'pass'])
  })

  it('(b) omitted item: span carries the FILLED (unsure) verdict, matching the return', async () => {
    const { span, verdicts } = await spanFor(
      fetchReturning(
        verdictsBody([{ item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'k' }])
      )
    )
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(verdicts.find(v => v.itemIndex === 2)?.verdict).toBe('unsure')
  })

  it('(c) unparseable body: span carries allUnsure, matching the return', async () => {
    const { span, verdicts } = await spanFor(fetchReturning('not json at all'))
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(verdicts.every(v => v.verdict === 'unsure')).toBe(true)
  })

  it('(d) error (network throw): span carries allUnsure, matching the return', async () => {
    const { span, verdicts } = await spanFor(fetchReturning(undefined, { throws: true }))
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(verdicts.every(v => v.verdict === 'unsure')).toBe(true)
  })
})
