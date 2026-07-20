// The trap-as-transformation live self-test (spec "End state"; INV-5). Rides
// every JUDGED round: apply each committed trap mutation on-the-fly to the
// round's OWN fresh captures, judge the doctored evidence, and assert a `fail`.
// Replaces the detection coverage the frozen doctored corpus provided, with zero
// fixture rot — the trap is the mutation, not the fixture.
//
// It tests the DETECTOR, not the app. Three hard-signal outcomes set exit 1: a
// MISSED trap (the judge did not `fail` doctored evidence), a NON-EXECUTABLE
// trap (target behavior/item absent, or the rendered-prompt liveness guard
// trips), and a per-trap EXCEPTION (caught + converted to `error`, never
// propagated so the report is still written).

import type { Capture } from '../support/types'
import type { Judge, PerItemVerdict } from './adapter/types'
import { buildPrompt } from './adapter/prompt'
import { applyMutation } from './doctor'
import { applyGrounding, groupByBehavior } from './grader/grade'
import type { CaptureSet, ItemVerdict } from './grader/types'
import { buildJudgeInput, judgeItemsFor } from './judge-input'
import type { TrapSpec } from './trap-config'

// The self-test NEVER attaches screenshots. The trap mutations (`doctor.ts`)
// only doctor STRUCTURED evidence (dialogs / API JSON / aria); a screenshot is
// pixels and cannot be doctored. Attaching the undoctored screenshot would hand
// the judge the real world alongside the doctored JSON — an escape hatch that
// lets it read the truth off the pixels and PASS, silently defeating any
// JSON/aria trap (observed: the DSH-004 stale-reason trap missed only when
// screenshots were on). Judging the doctored structured evidence alone keeps the
// detector test sound and deterministic.

// caught: the judge grounded-failed the doctored evidence (detection worked).
// missed: the judge did not grounded-fail it (detection gap). error: the trap
// could not execute (absent target, no-op mutation, or a thrown exception).
export type TrapStatus = 'caught' | 'missed' | 'error'

export interface TrapResult {
  id: string
  targetBehavior: string
  targetItem: number
  status: TrapStatus
  reason: string
}

function toItemVerdict(pv: PerItemVerdict): ItemVerdict {
  return { verdict: pv.verdict, citation: pv.citation, reason: pv.critique }
}

async function runOneTrap(sets: CaptureSet[], trap: TrapSpec, judge: Judge): Promise<TrapResult> {
  const base = { id: trap.id, targetBehavior: trap.targetBehavior, targetItem: trap.targetItem }
  try {
    // Absent target behavior → error (a configured trap that cannot execute is
    // a hard failure, NOT a benign skip — tour/tag drift must fail loudly).
    const set = sets.find(s => s.behaviorId === trap.targetBehavior)
    if (!set || set.captures.length === 0) {
      return {
        ...base,
        status: 'error',
        reason: `no captures for behavior ${trap.targetBehavior} in this round`,
      }
    }
    // The item must be judge-graded residue, else the judge short-circuits on
    // an empty items list and never fails.
    const item = judgeItemsFor(trap.targetBehavior).find(i => i.itemIndex === trap.targetItem)
    if (!item) {
      return {
        ...base,
        status: 'error',
        reason: `${trap.targetBehavior}[${trap.targetItem}] is not judge residue in this round`,
      }
    }

    // No resolveScreenshot: the self-test judges doctored structured evidence
    // only (see the file header) — undoctorable pixels would defeat the trap.
    const baseInput = buildJudgeInput(trap.targetBehavior, set.captures, [item])
    const mutated = applyMutation(set.captures, trap.mutation)
    const mutatedInput = buildJudgeInput(trap.targetBehavior, mutated, [item])
    if (!baseInput || !mutatedInput) {
      return { ...base, status: 'error', reason: `no spec for behavior ${trap.targetBehavior}` }
    }

    // Liveness guard on the RENDERED PROMPT (not raw captures): a mutation the
    // judge lane never projects (e.g. a `set_field`, or a target node that isn't
    // present) leaves the judge-visible evidence identical — a silent no-op. The
    // rendered-prompt diff is the true no-op guard.
    if (buildPrompt(baseInput) === buildPrompt(mutatedInput)) {
      return {
        ...base,
        status: 'error',
        reason: 'no-oped: rendered prompt unchanged (mutation invisible to the judge)',
      }
    }

    // Stamp the doctoring marker so the adapter forwards it onto the span
    // (`qa.mutation`). It reaches the adapter because we call the RAW judge with
    // our OWN input — the JudgeRunner would rebuild its input and drop `__trap`.
    mutatedInput.__trap = { mutation: trap.mutation }
    const verdicts = await judge(mutatedInput)

    const pv = verdicts.find(v => v.itemIndex === trap.targetItem)
    if (!pv) {
      return {
        ...base,
        status: 'missed',
        reason: `judge returned no verdict for item ${trap.targetItem}`,
      }
    }
    // Apply the PRODUCTION grounding rule before declaring caught: the report
    // downgrades an UNCITED fail to `unsure`, so an uncited fail on doctored
    // evidence is a MISS here too — the self-test's notion of "detected" matches
    // the production detector's, not a raw-fail shortcut.
    const grounded = applyGrounding(toItemVerdict(pv))
    if (grounded.verdict === 'fail') {
      return {
        ...base,
        status: 'caught',
        reason: grounded.citation ? `caught (cite: ${grounded.citation})` : 'caught',
      }
    }
    return {
      ...base,
      status: 'missed',
      reason: `judge returned ${grounded.verdict} (expected a grounded fail on doctored evidence)`,
    }
  } catch (e) {
    // Never propagate — a thrown trap must not abort the round before the report
    // is written (INV-5).
    return {
      ...base,
      status: 'error',
      reason: `threw: ${e instanceof Error ? e.message : String(e)}`,
    }
  }
}

// Run every trap serially over the round's captures (serial matches the residue
// path: concurrent codex spawns storm a quota-limited account). A RAW `Judge` is
// passed (not the JudgeRunner) so the `__trap` marker traverses to the adapter.
export async function runTrapSelfTest(
  captures: Capture[],
  traps: TrapSpec[],
  judge: Judge
): Promise<TrapResult[]> {
  const sets = groupByBehavior(captures)
  const results: TrapResult[] = []
  for (const trap of traps) {
    results.push(await runOneTrap(sets, trap, judge))
  }
  return results
}

// The hard-exit decision: 1 iff any trap missed or errored; 0 iff all caught.
// There is no "skip = exit 0" escape hatch — a configured trap that cannot
// execute is an `error`.
export function selftestExitCode(results: TrapResult[]): 0 | 1 {
  return results.some(r => r.status !== 'caught') ? 1 : 0
}
