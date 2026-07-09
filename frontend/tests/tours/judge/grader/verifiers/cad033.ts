// CAD-033 — unlinking is the only in-CRM mutation of a linked task.
//   [0] the CRM offers unlink (confirm), keeps task in remote  (provider → abstain)
//   [1] complete/dismiss happen in the remote app, not the CRM (provider → abstain)
//
// Both clauses need a provider-seeded linked task row to expose the unlink
// affordance (and to prove the absence of an in-CRM complete/dismiss). A
// provider-less sweep cannot reach that state, so both are skip-listed and the
// verifier abstains — the coverage report surfaces the reasons.

import type { CaptureSet, ItemVerdicts } from '../types'

export function cad033(_set: CaptureSet): ItemVerdicts {
  return {
    0: {
      verdict: 'unsure',
      reason:
        'the unlink row needs a provider-seeded linked task to expose it — abstain (skip-list)',
    },
    1: {
      verdict: 'unsure',
      reason:
        'a negative/absence claim over provider state (no in-CRM complete/dismiss); not tourable without provider-seeded tasks — abstain (skip-list)',
    },
  }
}
