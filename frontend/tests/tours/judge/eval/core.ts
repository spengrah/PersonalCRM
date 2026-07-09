// Eval core (pure): for each corpus case, apply its doctor mutation (if any),
// run the hybrid grader, compare predicted vs the self-labeled expected, and
// build the deterministic (verifier-item) confusion matrix + regression list.
// Judge/pending items never gate the merge (label-gated-deferred, design D7).
//
// Testable with in-memory cases + a capturesFor resolver — no fs, no YAML.

import type { Capture } from '../../support/types'
import type { Case } from '../corpus/schema'
import { applyMutation } from '../doctor'
import { gradeBehavior } from '../grader/grade'
import type { ItemVerdicts } from '../grader/types'
import { addToMatrix, emptyMatrix, type ConfusionMatrix, type Verdict } from './metrics'

export type JudgeRunner = (behaviorId: string, captures: Capture[]) => Promise<ItemVerdicts>

export interface EvalItemResult {
  thenIndex: number
  grader: 'verifier' | 'judge'
  source: 'verifier' | 'judge' | 'pending'
  expected: Verdict
  predicted: Verdict
  match: boolean
  citation?: string
  reason?: string
}

export interface EvalCaseResult {
  caseId: string
  behaviorId: string
  source: 'clean' | 'doctored'
  items: EvalItemResult[]
}

export interface EvalResult {
  cases: EvalCaseResult[]
  matrix: ConfusionMatrix // deterministic (verifier) items only — the merge gate
  regressions: EvalItemResult[] // verifier items where predicted !== expected
  judgeItemsPending: number // judge/residue items awaiting labels (N/A)
}

export async function runEval(
  cases: Case[],
  capturesFor: (c: Case) => Capture[],
  opts: { judge?: JudgeRunner } = {}
): Promise<EvalResult> {
  const matrix = emptyMatrix()
  const regressions: EvalItemResult[] = []
  const results: EvalCaseResult[] = []
  let judgeItemsPending = 0

  for (const c of cases) {
    let captures = capturesFor(c)
    if (c.source === 'doctored' && c.doctor) {
      captures = applyMutation(captures, c.doctor.mutation)
    }
    const set = { behaviorId: c.behavior_id, captures }
    const judgeVerdicts: ItemVerdicts | undefined = opts.judge
      ? await opts.judge(c.behavior_id, captures)
      : undefined
    const grade = gradeBehavior(set, { judge: judgeVerdicts })
    const byIndex = new Map(grade.items.map(i => [i.thenIndex, i]))

    const items: EvalItemResult[] = c.expected.map(exp => {
      const g = byIndex.get(exp.then_index)
      const predicted = (g?.verdict ?? 'unsure') as Verdict
      const item: EvalItemResult = {
        thenIndex: exp.then_index,
        grader: exp.grader,
        source: g?.source ?? 'pending',
        expected: exp.verdict,
        predicted,
        match: predicted === exp.verdict,
        citation: g?.citation,
        reason: g?.reason,
      }
      // The deterministic merge gate is over VERIFIER-sourced items only.
      if (item.source === 'verifier') {
        addToMatrix(matrix, item.expected, item.predicted)
        if (!item.match) regressions.push(item)
      } else {
        judgeItemsPending += 1
      }
      return item
    })
    results.push({ caseId: c.id, behaviorId: c.behavior_id, source: c.source, items })
  }

  return { cases: results, matrix, regressions, judgeItemsPending }
}
