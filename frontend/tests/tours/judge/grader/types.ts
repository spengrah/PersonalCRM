// Grader core types. The judge grades a behavior's residue then-items over the
// §1 Capture records from the merged tours harness (the deterministic verifier
// lane has migrated to Playwright E2E — see grade.ts + classification.ts).

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

// The captures a behavior grades: every capture tagging the behavior, in seq
// order.
export interface CaptureSet {
  behaviorId: string
  captures: Capture[]
}
