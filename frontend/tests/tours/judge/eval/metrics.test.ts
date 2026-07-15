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
import { cap, apiItem, pair } from '../grader/fixtures'
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

// A CAD-028 clean mark-contacted bracket (mutual POST on /dashboard) and its
// doctored twin. CAD-028 is a plain verifier (no judge/unbound rows), so it
// exercises the eval pipeline over deterministic verifier items only.
function cad028Captures(): Capture[] {
  return [
    cap({
      behaviors: ['CAD-028'],
      pair: pair('mc', 'mark-after'),
      url: '/dashboard',
      apiResponses: {
        'POST /api/v1/contacts/:id/interactions': [
          apiItem({
            method: 'POST',
            requestUrl: '/api/v1/contacts/<id:1>/interactions',
            status: 201,
            requestBody: { direction: 'mutual' },
            body: { data: { direction: 'mutual', occurred_at: '2026-07-12T15:00:00Z' } },
          }),
        ],
      },
    }),
  ]
}

const capturesByCase: Record<string, () => Capture[]> = {
  'CAD-028-clean': cad028Captures,
  'CAD-028-doctored': cad028Captures,
}

const cases: Case[] = [
  {
    id: 'CAD-028-clean',
    behavior_id: 'CAD-028',
    captures: ['x'],
    source: 'clean',
    expected: [{ then_index: 0, grader: 'verifier', verdict: 'pass' }],
  },
  {
    id: 'CAD-028-doctored',
    behavior_id: 'CAD-028',
    captures: ['x'],
    source: 'doctored',
    doctor: {
      base_case: 'CAD-028-clean',
      mutation: { op: 'delete_endpoint', endpoint: 'POST /api/v1/contacts/:id/interactions' },
    },
    expected: [{ then_index: 0, grader: 'verifier', verdict: 'fail' }],
  },
]

describe('runEval end-to-end on doctored self-labeled cases (verifiers-only)', () => {
  it('catches every doctored fail with zero collateral (no regressions)', async () => {
    const result = await runEval(cases, c => capturesByCase[c.id]())
    expect(result.regressions).toEqual([])
    // The doctored CAD-028[0] predicted fail (the deleted interaction POST is caught).
    const doctored = result.cases.find(c => c.caseId === 'CAD-028-doctored')
    expect(doctored?.items.find(i => i.thenIndex === 0)?.predicted).toBe('fail')
  })

  it('reports a regression when a doctored case is mislabeled (self-check)', async () => {
    const broken: Case[] = [
      {
        ...cases[0],
        id: 'CAD-028-broken',
        // Claim item 0 should FAIL on a CLEAN capture — the grader says pass → regression.
        expected: [{ then_index: 0, grader: 'verifier', verdict: 'fail' }],
      },
    ]
    const result = await runEval(broken, () => cad028Captures())
    expect(result.regressions.length).toBe(1)
  })
})
