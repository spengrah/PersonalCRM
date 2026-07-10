// Grader core types. A verifier is a PURE function over a behavior's captures
// (the §1 Capture record from the merged tours harness) that returns a
// per-then-item verdict. Verifiers never touch a model; the LLM judge owns only
// the residue (see grade.ts + classification.ts).

import type { Capture } from '../../support/types'

// Categorical only — no scores/Likert. `unsure` is abstention: it is routed to
// human review and is NEVER issue-eligible.
export type Verdict = 'pass' | 'fail' | 'unsure'

// Verifiers get a fourth outcome: `unbound` = the copy/structure ANCHOR the
// verifier binds through was not found in a PRESENT capture (plausibly renamed
// or moved — the redesign-brittleness class), distinct from bound-but-wrong
// (fail) and from ambiguous/missing evidence (unsure). Unbound items route to
// the item judge with the same evidence; unrouted (verifiers-only mode) they
// grade as unsure. Policy: PRESENCE-CONTRACT items — where the element's
// existence IS the asserted fact — deliberately keep fail-on-missing so the
// deterministic gate retains its true positives; only BINDING-VEHICLE anchors
// (copy strings incidental to the asserted fact) emit unbound.
export type VerifierVerdict = Verdict | 'unbound'

export interface ItemVerdict {
  verdict: Verdict
  /** The exact evidence bound: an aria node label, a JSON path, or a URL. */
  citation?: string
  /** Human-readable rationale (esp. for `unsure` capture-coverage caveats). */
  reason?: string
}

export interface VerifierItemVerdict {
  verdict: VerifierVerdict
  citation?: string
  reason?: string
}

// Keyed by then-item index (matching spec/contacts.yaml then-item order).
export type ItemVerdicts = Record<number, ItemVerdict>
export type VerifierItemVerdicts = Record<number, VerifierItemVerdict>

// The captures a verifier grades: every capture tagging the behavior, in seq
// order. A verifier looks them up by pair role or note.
export interface CaptureSet {
  behaviorId: string
  captures: Capture[]
}

export type Verifier = (set: CaptureSet) => VerifierItemVerdicts
