// DSH-005 — the overdue widget reflects out-of-flow membership changes.
//   [0] the overdue list refreshes without a manual reload
//   [1] covers interaction/merge/meeting-note   (caveat → abstain)
//   [2] cosmetic edits do not disturb the list   (caveat → abstain)
//   [3] refocus refetches only once stale (5-min) (caveat → abstain)
//
// [0]'s freshness OUTCOME is graded from the on-dashboard CAD-028 mark-contacted
// before/after: interaction:created invalidates the overdue query, which refetches
// on the SAME mounted dashboard (no full reload). The spec's "action elsewhere"
// breadth ([1]) + cosmetic-edit ([2]) + refocus-staleness ([3]) are not
// deterministically tourable from the dashboard → abstain with a caveat.

import { byRole, endpointItems, urlPathname } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

export function dsh005(set: CaptureSet): ItemVerdicts {
  const after = byRole(set, 'mark-after')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    if (!after) {
      return { verdict: 'unsure', reason: 'no mark-after capture — no freshness evidence' }
    }
    // The invalidation refetches the overdue query on the same dashboard (the
    // url is unchanged — no full navigation/reload). A present after-bracket with
    // NO overdue refetch is a real freshness defect.
    const refetched = endpointItems(after, 'GET /api/v1/contacts/overdue').length > 0
    const noReload = urlPathname(after.url) === '/dashboard'
    if (refetched && noReload) {
      return {
        verdict: 'pass',
        citation: 'after mark-contacted: overdue GET refetch on /dashboard (no reload)',
      }
    }
    if (!refetched) {
      return {
        verdict: 'fail',
        citation: 'mark-after bracket',
        reason: 'no overdue refetch after the interaction — the list did not refresh',
      }
    }
    return {
      verdict: 'unsure',
      reason: 'overdue refetched but the url changed — cannot confirm a no-reload refresh',
    }
  })()

  out[1] = {
    verdict: 'unsure',
    reason:
      'only interaction:created (mark-contacted) is dashboard-reachable; the merge and meeting-note-resolve triggers are other-surface flows — abstain (caveat)',
  }
  out[2] = {
    verdict: 'unsure',
    reason: 'a cosmetic-edit flow is not toured from the dashboard — abstain (caveat)',
  }
  out[3] = {
    verdict: 'unsure',
    reason:
      'the refocus / 5-minute-staleTime timing is not deterministically tourable — abstain (caveat)',
  }

  return out
}
