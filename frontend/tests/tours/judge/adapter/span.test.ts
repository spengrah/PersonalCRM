// The GenAI span carrier: the label-trace attributes (qa.scenario /
// qa.graded_evidence / qa.item_verdicts / qa.mutation) render only when the
// caller supplied them, and the existing gen_ai.* / qa.* attributes still gate.

import { describe, expect, it } from 'vitest'
import type { GradedEvidenceEntry, Scenario } from '../label-trace'
import { buildGenAiSpan, type SpanParams } from './span'
import type { PerItemVerdict } from './types'

const base: SpanParams = {
  impl: 'codex-exec',
  behaviorId: 'CON-042',
  model: 'gpt-5.4-mini',
  startMs: 1_000,
  endMs: 2_000,
}

const behaviorScenario: Scenario = {
  kind: 'behavior',
  behaviorId: 'CON-042',
  behaviorTitle: 'delete confirmation',
  given: 'g',
  when: 'w',
  items: [{ itemIndex: 0, thenText: 'warns cannot be undone' }],
  allThen: ['warns cannot be undone'],
}

const intentScenario: Scenario = {
  kind: 'intent',
  intentId: 'DSH-010',
  title: 'at a glance',
  statement: 'answers what needs attention',
  status: 'current',
}

const gradedEvidence: GradedEvidenceEntry[] = [
  { captureFile: '001.json', note: 'the dialog', evidence: { url: 'http://x/1' } },
]

const verdicts: PerItemVerdict[] = [
  { itemIndex: 0, verdict: 'fail', citation: 'dialog', critique: 'no warning' },
]

describe('buildGenAiSpan — label-trace attributes', () => {
  it('renders qa.scenario (behavior variant) / qa.graded_evidence / qa.item_verdicts when supplied', () => {
    const span = buildGenAiSpan({
      ...base,
      scenario: behaviorScenario,
      gradedEvidence,
      itemVerdicts: verdicts,
    })
    expect(span.attributes['qa.scenario']).toEqual(behaviorScenario)
    expect(span.attributes['qa.graded_evidence']).toEqual(gradedEvidence)
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
  })

  it('renders the intent scenario variant too', () => {
    const span = buildGenAiSpan({ ...base, scenario: intentScenario })
    expect(span.attributes['qa.scenario']).toEqual(intentScenario)
  })

  it('OMITS qa.mutation when undefined (real capture) and SETS it when doctored', () => {
    expect('qa.mutation' in buildGenAiSpan(base).attributes).toBe(false)
    const doctored = buildGenAiSpan({ ...base, mutation: { op: 'blank_dialog', target: {} } })
    expect(doctored.attributes['qa.mutation']).toEqual({ op: 'blank_dialog', target: {} })
  })

  it('omits every label-trace attribute for a metrics-only span (the #379 shape)', () => {
    const attrs = buildGenAiSpan(base).attributes
    for (const k of ['qa.scenario', 'qa.graded_evidence', 'qa.item_verdicts', 'qa.mutation']) {
      expect(k in attrs).toBe(false)
    }
  })

  it('still renders the existing gen_ai.* / qa.* attributes + content gating', () => {
    const span = buildGenAiSpan({
      ...base,
      inputTokens: 100,
      outputTokens: 20,
      finishReasons: ['stop'],
      toolRejected: false,
      prompt: 'the prompt',
      response: 'the response',
      screenshots: ['/a.png'],
    })
    expect(span.attributes['gen_ai.operation.name']).toBe('chat')
    expect(span.attributes['gen_ai.request.model']).toBe('gpt-5.4-mini')
    expect(span.attributes['qa.behavior_id']).toBe('CON-042')
    expect(span.attributes['qa.tool_rejected']).toBe(false)
    expect(span.attributes['gen_ai.usage.input_tokens']).toBe(100)
    expect(span.attributes['gen_ai.prompt']).toBe('the prompt')
    expect(span.attributes['gen_ai.completion']).toBe('the response')
    expect(span.attributes['qa.screenshots']).toEqual(['/a.png'])
    // Content stays call-site opt-in: a bare span carries none of it.
    const bare = buildGenAiSpan(base).attributes
    expect('gen_ai.prompt' in bare).toBe(false)
    expect('qa.screenshots' in bare).toBe(false)
  })
})
