// DSH-001 — the dashboard is the application's default landing surface.
//   [0] taken to the dashboard as the default destination (landing url /dashboard)
//   [1] a brief loading indicator shows during the redirect
//
// [1] reads the transient redirect spinner best-effort (fields.rootSpinnerSeen,
// D2a): the spinner is aria-invisible and can vanish before the read, so the
// verifier PASSES iff it was observed and otherwise ABSTAINS — never a false
// fail (the spinner racing the redirect is not a defect).

import { byRole, findByRoleName, urlPathname } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

export function dsh001(set: CaptureSet): ItemVerdicts {
  const landing = byRole(set, 'landing')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    if (!landing) return { verdict: 'unsure', reason: 'no landing capture — no evidence' }
    const onDashboard = urlPathname(landing.url) === '/dashboard'
    const dashHeading = findByRoleName(landing.aria, 'heading', 'Action Required')
    if (onDashboard && dashHeading) {
      return {
        verdict: 'pass',
        citation: "landing url pathname /dashboard + 'Action Required' heading",
      }
    }
    return {
      verdict: 'fail',
      citation: 'landing url + dashboard heading',
      reason: `the app root did not land on the dashboard (pathname=${urlPathname(landing.url)}, Action Required heading ${dashHeading ? 'present' : 'absent'})`,
    }
  })()

  out[1] = ((): ItemVerdict => {
    if (!landing) return { verdict: 'unsure', reason: 'no landing capture — no evidence' }
    if (landing.fields?.rootSpinnerSeen === true) {
      return { verdict: 'pass', citation: 'fields.rootSpinnerSeen (redirect spinner observed)' }
    }
    return {
      verdict: 'unsure',
      reason:
        'the redirect spinner was not observed (best-effort: the transient spinner likely out-raced the read)',
    }
  })()

  return out
}
