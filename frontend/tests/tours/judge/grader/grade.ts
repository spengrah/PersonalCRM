// The grader: map the LLM judge's residue verdicts onto the behavior's
// classification rows, apply the grounding rule, and aggregate to a behavior
// verdict. The deterministic verifier lane has migrated to Playwright E2E, so
// only the judge-tagged residue (CON-042[0], DSH-004[2]) is graded here.

import { classificationFor, type GraderKind } from './classification'
import type { CaptureSet, ItemVerdict, ItemVerdicts, Verdict } from './types'
import type { Capture } from '../../support/types'

export type VerdictSource = 'verifier' | 'judge' | 'pending'

export interface GradedItem {
  thenIndex: number
  grader: GraderKind
  source: VerdictSource
  verdict: Verdict
  citation?: string
  reason?: string
}

export interface BehaviorGrade {
  behaviorId: string
  behaviorVerdict: Verdict
  items: GradedItem[]
}

export interface GradeOptions {
  // Judge verdicts for the behavior's judge-tagged residue items, keyed by
  // then index. Absent until the judge runs.
  judge?: ItemVerdicts
}

function hasCitation(v: ItemVerdict): boolean {
  return typeof v.citation === 'string' && v.citation.trim() !== ''
}

// Grounding rule (D4): a `fail` with no resolvable citation is downgraded to
// `unsure` — applied to the judge's verdicts after parsing.
export function applyGrounding(v: ItemVerdict): ItemVerdict {
  if (v.verdict === 'fail' && !hasCitation(v)) {
    return {
      verdict: 'unsure',
      reason: `${v.reason ?? 'fail'} — downgraded to unsure: no grounding citation`,
    }
  }
  return v
}

export function gradeBehavior(set: CaptureSet, opts: GradeOptions = {}): BehaviorGrade {
  const classification = classificationFor(set.behaviorId)
  const judge = opts.judge ?? {}

  const items: GradedItem[] = classification.map(c => {
    const idx = c.thenIndex
    const jv = judge[idx]
    if (jv) {
      const grounded = applyGrounding(jv)
      return { thenIndex: idx, grader: c.grader, source: 'judge', ...grounded }
    }
    return {
      thenIndex: idx,
      grader: c.grader,
      source: 'pending',
      verdict: 'unsure',
      reason: 'judge-only item — no judge verdict supplied',
    }
  })

  return {
    behaviorId: set.behaviorId,
    behaviorVerdict: aggregate(items.map(i => i.verdict)),
    items,
  }
}

// any item fail → behavior fail; all pass → pass; else (some abstained) → unsure.
export function aggregate(verdicts: Verdict[]): Verdict {
  if (verdicts.some(v => v === 'fail')) return 'fail'
  if (verdicts.length > 0 && verdicts.every(v => v === 'pass')) return 'pass'
  return 'unsure'
}

// Group a flat capture list into per-behavior CaptureSets (a capture may tag
// several behaviors — it lands in each). Deterministic key order.
export function groupByBehavior(captures: Capture[]): CaptureSet[] {
  const byId = new Map<string, Capture[]>()
  for (const cap of captures) {
    for (const b of cap.behaviors) {
      const list = byId.get(b) ?? []
      list.push(cap)
      byId.set(b, list)
    }
  }
  return [...byId.keys()]
    .sort()
    .map(behaviorId => ({ behaviorId, captures: byId.get(behaviorId) ?? [] }))
}
