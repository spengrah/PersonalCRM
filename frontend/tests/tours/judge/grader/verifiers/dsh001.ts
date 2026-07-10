// DSH-001 — the dashboard is the application's default landing surface.
//   [0] taken to the dashboard as the default destination (landing url /dashboard)
//   (a former [1] pinned the redirect's interim presentation; the maintainer
//   retired that clause — interim quality is judged holistically under DSH-011)

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

  return out
}
