// The intent pass: one judge call per intent over the union of its bound
// captures (see intent-input.ts). Sibling of the item-judge residue path —
// never part of the offline merge gate; verdicts are advisory and label-gated
// like the rest of the judge layer.

import { selectJudge } from './adapter'
import { makeCodexExecJudge } from './adapter/codex-exec'
import type { Judge } from './adapter'
import { applyGrounding } from './grader/grade'
import type { Verdict } from './grader/types'
import { allIntents, type IntentSpec, type IntentStatus } from './intent-catalog'
import { bindIntentCaptures, buildIntentJudgeInput, INTENT_CAPTURE_CAP } from './intent-input'
import type { Capture } from '../support/types'

export interface IntentGrade {
  intentId: string
  title: string
  status: IntentStatus
  verdict: Verdict
  citation?: string
  reason?: string
  boundCount: number
  droppedCount: number
  servedBy: string[]
}

// Intent judgment is the semantically hard task and the call count is small
// (~one per intent per run), so it defaults to a stronger model + effort than
// the cheap item-residue judge. Overridable via QA_INTENT_MODEL /
// QA_INTENT_EFFORT; QA_JUDGE still selects the adapter kind.
export const DEFAULT_INTENT_MODEL = 'gpt-5.5'
export const DEFAULT_INTENT_EFFORT = 'medium'

export function makeIntentJudge(kind: string = process.env.QA_JUDGE ?? 'codex-exec'): Judge {
  const model = process.env.QA_INTENT_MODEL ?? DEFAULT_INTENT_MODEL
  if (kind === 'codex-exec') {
    const effort = process.env.QA_INTENT_EFFORT ?? DEFAULT_INTENT_EFFORT
    return makeCodexExecJudge({ model, effort })
  }
  return selectJudge(kind, model)
}

// Run the pass SERIALLY (matching the residue path: concurrent codex spawns
// storm a quota-limited account). A zero-evidence intent abstains WITHOUT a
// model call — a freshly minted design-session intent is visibly unjudgeable,
// not silently absent.
export async function runIntentPass(
  captures: Capture[],
  judge: Judge,
  intents: IntentSpec[] = allIntents(),
  cap: number = INTENT_CAPTURE_CAP
): Promise<IntentGrade[]> {
  const grades: IntentGrade[] = []
  for (const intent of intents) {
    const { captures: bound, dropped } = bindIntentCaptures(intent, captures, cap)
    const base = {
      intentId: intent.id,
      title: intent.title,
      status: intent.status,
      boundCount: bound.length,
      droppedCount: dropped,
      servedBy: intent.servedBy,
    }
    if (bound.length === 0) {
      grades.push({
        ...base,
        verdict: 'unsure',
        reason: 'no evidence bound — no capture tags this intent or a serving behavior',
      })
      continue
    }
    const verdicts = await judge(buildIntentJudgeInput(intent, bound))
    const v = verdicts.find(x => x.itemIndex === 0)
    if (!v) {
      grades.push({ ...base, verdict: 'unsure', reason: 'no verdict returned' })
      continue
    }
    const grounded = applyGrounding({
      verdict: v.verdict,
      citation: v.citation,
      reason: v.critique,
    })
    grades.push({ ...base, ...grounded })
  }
  return grades
}
