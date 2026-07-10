// The hybrid grader: run the deterministic verifiers, merge the LLM judge's
// residue verdicts, apply the grounding rule, and aggregate to a behavior
// verdict. "Verifiers before judges" — the judge only supplies judge-tagged
// items (and verifier items that abstained AND carry judgeFallback).

import { classificationFor, type GraderKind } from './classification'
import type { CaptureSet, ItemVerdict, ItemVerdicts, Verdict, VerifierItemVerdicts } from './types'
import { VERIFIERS } from './verifiers'
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
  // Judge verdicts for the behavior's judge-tagged / residue items, keyed by
  // then index. Absent in verifiers-only mode.
  judge?: ItemVerdicts
}

function hasCitation(v: ItemVerdict): boolean {
  return typeof v.citation === 'string' && v.citation.trim() !== ''
}

// Grounding rule (D4): a `fail` with no resolvable citation is downgraded to
// `unsure` — applied to the MODEL's verdicts after parsing (verifier verdicts
// carry citations by construction, but we apply it uniformly for safety).
export function applyGrounding(v: ItemVerdict): ItemVerdict {
  if (v.verdict === 'fail' && !hasCitation(v)) {
    return {
      verdict: 'unsure',
      reason: `${v.reason ?? 'fail'} — downgraded to unsure: no grounding citation`,
    }
  }
  return v
}

// Run the deterministic verifiers alone (the two-phase judge runner uses this
// to learn which items unbound before asking the judge).
export function runVerifiers(set: CaptureSet): VerifierItemVerdicts {
  const verifier = VERIFIERS[set.behaviorId]
  return verifier ? verifier(set) : {}
}

export function gradeBehavior(set: CaptureSet, opts: GradeOptions = {}): BehaviorGrade {
  const classification = classificationFor(set.behaviorId)
  const verifierVerdicts = runVerifiers(set)
  const judge = opts.judge ?? {}

  const items: GradedItem[] = classification.map(c => {
    const idx = c.thenIndex
    if (c.grader === 'judge') {
      const jv = judge[idx]
      if (jv) {
        const grounded = applyGrounding(jv)
        return { thenIndex: idx, grader: 'judge', source: 'judge', ...grounded }
      }
      return {
        thenIndex: idx,
        grader: 'judge',
        source: 'pending',
        verdict: 'unsure',
        reason: 'judge-only item — no judge verdict (verifiers-only mode)',
      }
    }

    // verifier item
    const vv = verifierVerdicts[idx] ?? {
      verdict: 'unsure' as Verdict,
      reason: 'no verifier verdict emitted',
    }
    if (vv.verdict === 'unbound') {
      // The binding-vehicle anchor was not found in present evidence: route to
      // the judge (which reasons over the same aria without the copy pin).
      if (judge[idx]) {
        const grounded = applyGrounding(judge[idx])
        return { thenIndex: idx, grader: 'verifier', source: 'judge', ...grounded }
      }
      return {
        thenIndex: idx,
        grader: 'verifier',
        source: 'pending',
        verdict: 'unsure',
        reason: `${vv.reason ?? 'anchor unbound'} — judge routing pending (verifiers-only mode)`,
      }
    }
    return { thenIndex: idx, grader: 'verifier', source: 'verifier', ...(vv as ItemVerdict) }
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
