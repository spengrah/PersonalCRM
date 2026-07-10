// DSH-004 — the overdue widget distinguishes loading + error from its content.
//   [0] loading → placeholder, not empty/caught-up (route-held capture, D2a)
//   [1] failure → an error state with a reason, not empty/caught-up (route-500
//       capture). The 'Error loading overdue contacts' heading is a BINDING
//       VEHICLE: absent WITHOUT wrong-state signals → unbound → judge (the
//       error surface may be renamed/restyled); caught-up/cards on a failure
//       stay a deterministic fail.
//   [2] reason-faithfulness — JUDGE-owned (no verifier)
//
// [0]'s loading skeletons are anonymous animate-pulse divs (no aria role/name),
// so the verifier binds the tour's fields.overdueLoadingSkeletons count AND the
// discriminating negative (no caught-up text, no overdue cards while held). A
// skeleton count of 0 is ambiguous (a racy read) → abstain, never false-pass.

import { ariaTextIncludes, byRole, findByRoleName } from '../evidence'
import type { CaptureSet, VerifierItemVerdict, VerifierItemVerdicts } from '../types'
import type { AriaNode } from '../../../support/types'

function fieldNumber(fields: Record<string, unknown> | undefined, key: string): number | undefined {
  const v = fields?.[key]
  return typeof v === 'number' ? v : undefined
}

function hasOverdueCards(aria: AriaNode): boolean {
  return findByRoleName(aria, 'button', 'Mark as Contacted') !== undefined
}

export function dsh004(set: CaptureSet): VerifierItemVerdicts {
  const loading = byRole(set, 'loading')
  const error = byRole(set, 'error')
  const out: VerifierItemVerdicts = {}

  out[0] = ((): VerifierItemVerdict => {
    if (!loading) return { verdict: 'unsure', reason: 'no loading capture — no evidence' }
    const skeletons = fieldNumber(loading.fields, 'overdueLoadingSkeletons')
    if (skeletons === undefined) {
      return {
        verdict: 'unsure',
        reason: 'no fields.overdueLoadingSkeletons — placeholder not captured',
      }
    }
    const caughtUp = ariaTextIncludes(loading.aria, 'All caught up')
    const cards = hasOverdueCards(loading.aria)
    if (skeletons === 0) {
      // Ambiguous: the read may have raced the skeleton's removal → abstain.
      return {
        verdict: 'unsure',
        reason:
          'no loading skeletons observed (best-effort; the read may have out-raced the placeholder)',
      }
    }
    if (caughtUp || cards) {
      return {
        verdict: 'fail',
        citation: 'loading capture aria',
        reason:
          'the loading state showed a caught-up/populated state instead of a pure placeholder',
      }
    }
    return {
      verdict: 'pass',
      citation: `fields.overdueLoadingSkeletons=${skeletons} + no caught-up/cards (placeholder shown)`,
    }
  })()

  out[1] = ((): VerifierItemVerdict => {
    if (!error) return { verdict: 'unsure', reason: 'no error capture — no evidence' }
    const errShown = ariaTextIncludes(error.aria, 'Error loading overdue contacts')
    const caughtUp = ariaTextIncludes(error.aria, 'All caught up')
    const cards = hasOverdueCards(error.aria)
    if (errShown && !caughtUp) {
      return {
        verdict: 'pass',
        citation: "'Error loading overdue contacts' error state (not caught-up)",
      }
    }
    if (caughtUp || cards) {
      return {
        verdict: 'fail',
        citation: 'error capture aria',
        reason:
          'the request failure surfaced a caught-up/populated state instead of an error state',
      }
    }
    // The heading copy is a BINDING VEHICLE, not the asserted fact: the spec
    // clause asserts "an error state with a failure reason is shown", which a
    // renamed/restyled error surface (e.g. the bare reason text) can still
    // satisfy. No wrong-state signal is present, so route to the judge.
    return {
      verdict: 'unbound',
      reason:
        "the 'Error loading overdue contacts' heading anchor is absent with no caught-up/cards shown — the error surface may be renamed/restyled; judge decides from the failure bracket",
    }
  })()

  return out
}
