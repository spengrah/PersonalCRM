// Bridge: a Judge adapter → a per-behavior runner the grader/eval/report consume.
// Shared by the eval's --judge advisory layer AND the live-capture report, so
// both wire the judge identically (build the residue evidence bundle, call the
// judge, map its per-item verdicts into the grader's ItemVerdicts shape). The
// judge is only invoked when a behavior actually has residue items.

import { selectJudge } from './adapter'
import type { Judge } from './adapter'
import { buildJudgeInput } from './judge-input'
import type { Capture } from '../support/types'
import type { ItemVerdicts } from './grader/types'

// (behaviorId, captures) → judge verdicts keyed by then-index. Empty ({}) when
// the behavior has no residue items — no judge call is made in that case.
export type JudgeRunner = (behaviorId: string, captures: Capture[]) => Promise<ItemVerdicts>

// Wrap a concrete Judge. Pure w.r.t. selection — testable with a fake judge.
export function runnerFromJudge(judge: Judge): JudgeRunner {
  return async (behaviorId: string, captures: Capture[]): Promise<ItemVerdicts> => {
    const input = buildJudgeInput(behaviorId, captures)
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
  profile: string = process.env.QA_JUDGE ?? 'codex-exec'
): JudgeRunner {
  return runnerFromJudge(selectJudge(profile))
}
