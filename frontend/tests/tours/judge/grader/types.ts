// Grader core types. A verifier is a PURE function over a behavior's captures
// (the §1 Capture record from the merged tours harness) that returns a
// per-then-item verdict. Verifiers never touch a model; the LLM judge owns only
// the residue (see grade.ts + classification.ts).

import type { Capture } from '../../support/types'

// Categorical only — no scores/Likert. `unsure` is abstention: it is routed to
// human review and is NEVER issue-eligible.
export type Verdict = 'pass' | 'fail' | 'unsure'

export interface ItemVerdict {
  verdict: Verdict
  /** The exact evidence bound: an aria node label, a JSON path, or a URL. */
  citation?: string
  /** Human-readable rationale (esp. for `unsure` capture-coverage caveats). */
  reason?: string
}

// Keyed by then-item index (matching spec/contacts.yaml then-item order).
export type ItemVerdicts = Record<number, ItemVerdict>

// The captures a verifier grades: every capture tagging the behavior, in seq
// order. A verifier looks them up by pair role or note.
export interface CaptureSet {
  behaviorId: string
  captures: Capture[]
}

export type Verifier = (set: CaptureSet) => ItemVerdicts
