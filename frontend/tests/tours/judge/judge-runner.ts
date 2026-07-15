// Bridge: a Judge adapter → a per-behavior runner the grader/report consume.
// Shared by the advisory report's --judge layer, so both wire the judge
// identically. The judge grades the behavior's judge-tagged residue items and
// is only invoked when the behavior actually has residue items.

import { selectJudge } from './adapter'
import type { Judge } from './adapter'
import { buildJudgeInput, judgeItemsFor } from './judge-input'
import type { Capture } from '../support/types'
import type { ItemVerdicts } from './grader/types'
import type { ScreenshotResolver } from './intent-input'

// (behaviorId, captures) → judge verdicts keyed by then-index. Empty ({}) when
// the behavior has no residue items — no judge call is made in that case.
export type JudgeRunner = (behaviorId: string, captures: Capture[]) => Promise<ItemVerdicts>

// Wrap a concrete Judge. Pure w.r.t. selection — testable with a fake judge.
export function runnerFromJudge(judge: Judge, resolveScreenshot?: ScreenshotResolver): JudgeRunner {
  return async (behaviorId: string, captures: Capture[]): Promise<ItemVerdicts> => {
    const items = judgeItemsFor(behaviorId)
    const input = buildJudgeInput(behaviorId, captures, items, resolveScreenshot)
    if (!input || input.items.length === 0) return {}
    const verdicts = await judge(input)
    const out: ItemVerdicts = {}
    for (const v of verdicts)
      out[v.itemIndex] = { verdict: v.verdict, citation: v.citation, reason: v.critique }
    return out
  }
}

// Select the judge by the QA_JUDGE env (default codex-exec) and wrap it.
export function makeJudgeRunner(
  profile: string = process.env.QA_JUDGE ?? 'codex-exec',
  resolveScreenshot?: ScreenshotResolver
): JudgeRunner {
  return runnerFromJudge(selectJudge(profile), resolveScreenshot)
}
