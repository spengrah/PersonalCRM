// The intent pass: one judge call per intent over the union of its bound
// captures (see intent-input.ts). Sibling of the item-judge residue path —
// advisory and label-gated like the rest of the judge layer, never a blocking
// check.

import { selectJudge } from './adapter'
import { makeCodexExecJudge } from './adapter/codex-exec'
import { makeCodexSdkJudge } from './adapter/codex-sdk'
import type { Judge } from './adapter'
import { applyGrounding } from './grader/grade'
import type { Verdict } from './grader/types'
import { allIntents, type IntentSpec, type IntentStatus } from './intent-catalog'
import {
  bindIntentCaptures,
  buildIntentJudgeInput,
  INTENT_CAPTURE_CAP,
  type ScreenshotResolver,
} from './intent-input'
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
  /** The intent is visual (catalog flag) but was judged without screenshots. */
  ariaOnly?: boolean
}

// Intent judgment is the semantically hard task and the call count is small
// (~one per intent per run), so it defaults to a stronger model + effort than
// the cheap item-residue judge. Overridable via QA_INTENT_MODEL /
// QA_INTENT_EFFORT; QA_JUDGE still selects the adapter kind.
export const DEFAULT_INTENT_MODEL = 'gpt-5.5'
export const DEFAULT_INTENT_EFFORT = 'medium'

// A fail citation is grounded iff it names at least one IN-RANGE CAPTURE[n]
// index AND carries a node label / JSON path beyond the marker(s) — the
// residue must contain a word character (allow-list), so punctuation-only
// tails like "CAPTURE[0]." never ground. Deliberately a flat heuristic: the
// in-range and residue checks are independent over the whole string (no
// marker↔residue association — the model's citation syntax isn't guaranteed),
// and its worst failure mode is a conservative fail→unsure downgrade.
// Pure; exported for tests.
export function isGroundedIntentCitation(citation: string, boundCount: number): boolean {
  const indices = [...citation.matchAll(/CAPTURE\[(\d+)\]/g)].map(m => Number(m[1]))
  if (indices.length === 0 || !indices.some(i => i < boundCount)) return false
  const residue = citation.replace(/CAPTURE\[\d+\]/g, ' ')
  return /[\p{L}\p{N}]/u.test(residue)
}

export function makeIntentJudge(kind: string = process.env.QA_JUDGE ?? 'codex-exec'): Judge {
  // Both codex adapters drive the same engine and take the same {model, effort}
  // options, so the stronger intent model + effort apply to each.
  if (kind === 'codex-exec' || kind === 'codex-sdk') {
    const model = process.env.QA_INTENT_MODEL ?? DEFAULT_INTENT_MODEL
    const effort = process.env.QA_INTENT_EFFORT ?? DEFAULT_INTENT_EFFORT
    return kind === 'codex-exec'
      ? makeCodexExecJudge({ model, effort })
      : makeCodexSdkJudge({ model, effort })
  }
  // Non-codex adapters own their model config (e.g. QA_JUDGE_HTTP_MODEL) and
  // an explicit opts.model would override it — so the codex-oriented
  // DEFAULT_INTENT_MODEL must not leak onto other endpoints. Only an explicit
  // QA_INTENT_MODEL overrides.
  return selectJudge(kind, process.env.QA_INTENT_MODEL)
}

// Run the pass SERIALLY (matching the residue path: concurrent codex spawns
// storm a quota-limited account). A zero-evidence intent abstains WITHOUT a
// model call — a freshly minted design-session intent is visibly unjudgeable,
// not silently absent.
export async function runIntentPass(
  captures: Capture[],
  judge: Judge,
  intents: IntentSpec[] = allIntents(),
  cap: number = INTENT_CAPTURE_CAP,
  resolveScreenshot?: ScreenshotResolver
): Promise<IntentGrade[]> {
  const grades: IntentGrade[] = []
  for (const intent of intents) {
    const { captures: bound, dropped } = bindIntentCaptures(intent, captures, cap)
    const input = buildIntentJudgeInput(intent, bound, resolveScreenshot)
    const base = {
      intentId: intent.id,
      title: intent.title,
      status: intent.status,
      boundCount: bound.length,
      droppedCount: dropped,
      servedBy: intent.servedBy,
      ...(intent.visual && (input.images?.length ?? 0) === 0 ? { ariaOnly: true } : {}),
    }
    if (bound.length === 0) {
      grades.push({
        ...base,
        verdict: 'unsure',
        reason: 'no evidence bound — no capture tags this intent or a serving behavior',
      })
      continue
    }
    const verdicts = await judge(input)
    const v = verdicts.find(x => x.itemIndex === 0)
    if (!v) {
      grades.push({ ...base, verdict: 'unsure', reason: 'no verdict returned' })
      continue
    }
    let grounded = applyGrounding({
      verdict: v.verdict,
      citation: v.citation,
      reason: v.critique,
    })
    // Intent grounding is stricter than the generic rule: the prompt requires
    // a fail to name the capture index it bound to PLUS the node/path within
    // it — a bare marker, an out-of-range index, or a missing marker cannot
    // anchor a regression/progress signal.
    if (
      grounded.verdict === 'fail' &&
      !isGroundedIntentCitation(grounded.citation ?? '', bound.length)
    ) {
      grounded = {
        verdict: 'unsure',
        reason: `${grounded.reason ?? 'fail'} — downgraded to unsure: citation needs an in-range CAPTURE[n] index plus the node/path it binds to`,
      }
    }
    grades.push({ ...base, ...grounded })
  }
  return grades
}
