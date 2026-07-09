import { describe, it, expect } from 'vitest'
import {
  abstentionRate,
  addToMatrix,
  emptyMatrix,
  precisionRecall,
  selfConsistency,
  total,
} from './metrics'
import { runEval } from './core'
import { cap, apiItem, pair, root } from '../grader/fixtures'
import type { Case } from '../corpus/schema'
import type { Capture } from '../../support/types'

describe('confusion matrix + metrics', () => {
  it('computes precision/recall/abstention from a matrix', () => {
    const m = emptyMatrix()
    addToMatrix(m, 'pass', 'pass')
    addToMatrix(m, 'pass', 'pass')
    addToMatrix(m, 'fail', 'fail')
    addToMatrix(m, 'fail', 'unsure') // one fail predicted unsure
    addToMatrix(m, 'unsure', 'unsure')
    expect(total(m)).toBe(5)
    const failPR = precisionRecall(m, 'fail')
    expect(failPR.precision).toBe(1) // 1 predicted fail, 1 correct
    expect(failPR.recall).toBeCloseTo(0.5) // 2 expected fail, 1 recalled
    expect(abstentionRate(m)).toBeCloseTo(2 / 5)
  })

  it('null precision/recall when a verdict never appears', () => {
    const m = emptyMatrix()
    addToMatrix(m, 'pass', 'pass')
    expect(precisionRecall(m, 'fail')).toEqual({ precision: null, recall: null })
  })
})

describe('selfConsistency', () => {
  it('1.0 when every run agrees; lower on disagreement', () => {
    expect(
      selfConsistency([
        ['pass', 'fail'],
        ['pass', 'fail'],
      ])
    ).toBe(1)
    expect(
      selfConsistency([
        ['pass', 'fail'],
        ['pass', 'unsure'],
      ])
    ).toBe(0.5)
    expect(selfConsistency([['pass']])).toBe(1) // single run
  })
})

// --- End-to-end eval on in-memory doctored self-labeled cases ---

// A CON-041 clean bracket (edit + merge captures) and its doctored twin.
function con041Captures(): Capture[] {
  return [
    cap({
      behaviors: ['CON-041'],
      note: 'action=edit consumed once and stripped from URL',
      url: '/contacts/<id:1>?sort=cadence&order=desc',
      aria: root([{ role: 'heading', name: 'Edit Contact', level: 2 }]),
    }),
    cap({
      behaviors: ['CON-041'],
      note: 'action=merge consumed once and stripped from URL',
      url: '/contacts/<id:1>?sort=cadence&order=desc',
      aria: root([{ role: 'heading', name: 'Merge Contacts', level: 2 }]),
    }),
  ]
}

function con044Captures(): Capture[] {
  return [
    cap({
      behaviors: ['CON-044'],
      pair: pair('mc', 'after'),
      apiResponses: {
        'POST /api/v1/contacts/:id/interactions': [
          apiItem({
            method: 'POST',
            status: 201,
            requestBody: { direction: 'mutual' },
            body: { data: { direction: 'mutual', occurred_at: '2026-07-12T16:14:34Z' } },
          }),
        ],
      },
    }),
  ]
}

const capturesByCase: Record<string, () => Capture[]> = {
  'CON-041-clean': con041Captures,
  'CON-041-doctored': con041Captures,
  'CON-044-clean': con044Captures,
  'CON-044-doctored': con044Captures,
}

const cases: Case[] = [
  {
    id: 'CON-041-clean',
    behavior_id: 'CON-041',
    captures: ['x'],
    source: 'clean',
    expected: [
      { then_index: 0, grader: 'verifier', verdict: 'pass' },
      { then_index: 1, grader: 'verifier', verdict: 'pass' },
    ],
  },
  {
    id: 'CON-041-doctored',
    behavior_id: 'CON-041',
    captures: ['x'],
    source: 'doctored',
    doctor: {
      base_case: 'CON-041-clean',
      mutation: { op: 'inject_query', param: 'action', value: 'edit' },
    },
    expected: [
      { then_index: 0, grader: 'verifier', verdict: 'pass' },
      { then_index: 1, grader: 'verifier', verdict: 'fail' }, // self-labeled by the mutation
    ],
  },
  {
    id: 'CON-044-doctored',
    behavior_id: 'CON-044',
    captures: ['x'],
    source: 'doctored',
    doctor: {
      base_case: 'CON-044-clean',
      mutation: { op: 'delete_endpoint', endpoint: 'POST /api/v1/contacts/:id/interactions' },
    },
    expected: [{ then_index: 0, grader: 'verifier', verdict: 'fail' }],
  },
]

describe('runEval end-to-end on doctored self-labeled cases (verifiers-only)', () => {
  it('catches every doctored fail with zero collateral (no regressions)', async () => {
    const result = await runEval(cases, c => capturesByCase[c.id]())
    expect(result.regressions).toEqual([])
    // The doctored CON-041[1] and CON-044[0] predicted fail (caught).
    const doctored041 = result.cases.find(c => c.caseId === 'CON-041-doctored')
    expect(doctored041?.items.find(i => i.thenIndex === 1)?.predicted).toBe('fail')
    const doctored044 = result.cases.find(c => c.caseId === 'CON-044-doctored')
    expect(doctored044?.items[0].predicted).toBe('fail')
  })

  it('reports a regression when a doctored case is mislabeled (self-check)', async () => {
    const broken: Case[] = [
      {
        ...cases[0],
        id: 'CON-041-broken',
        // Claim item 1 should FAIL on a CLEAN capture — the grader says pass → regression.
        expected: [
          { then_index: 0, grader: 'verifier', verdict: 'pass' },
          { then_index: 1, grader: 'verifier', verdict: 'fail' },
        ],
      },
    ]
    const result = await runEval(broken, () => con041Captures())
    expect(result.regressions.length).toBe(1)
  })
})
