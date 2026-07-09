// DSH-002 — every primary surface is reachable from a persistent global nav.
//   [0] nav links to dashboard/contacts/birthdays/imports/settings present (>= sm)
//   [1] the active link is visually marked (fields.activeNavClass — aria-invisible)
//   [2] the nav stays visible while scrolling (fields.navPosition === sticky)
//
// [1]/[2] are CSS/DOM-only state the aria tree cannot express, so they bind to
// the tour's targeted fields reads (D2a).

import { asString, byRole, findByRoleName } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

const NAV_LINKS = ['Dashboard', 'Contacts', 'Birthdays', 'Imports', 'Settings']

export function dsh002(set: CaptureSet): ItemVerdicts {
  const nav = byRole(set, 'dashboard')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    if (!nav) return { verdict: 'unsure', reason: 'no dashboard capture — no evidence' }
    const missing = NAV_LINKS.filter(name => !findByRoleName(nav.aria, 'link', name))
    return missing.length === 0
      ? { verdict: 'pass', citation: `nav links present: ${NAV_LINKS.join(', ')}` }
      : {
          verdict: 'fail',
          citation: 'nav aria links',
          reason: `global nav is missing link(s): ${missing.join(', ')}`,
        }
  })()

  out[1] = ((): ItemVerdict => {
    if (!nav) return { verdict: 'unsure', reason: 'no dashboard capture — no evidence' }
    const cls = asString(nav.fields?.activeNavClass)
    if (cls === undefined) {
      return { verdict: 'unsure', reason: 'no fields.activeNavClass — active mark not captured' }
    }
    // The active section link carries the border-blue-500 active mark
    // (navigation.tsx). A different class means the active state is not marked.
    return cls.includes('border-blue-500')
      ? {
          verdict: 'pass',
          citation: 'fields.activeNavClass carries the active mark (border-blue-500)',
        }
      : {
          verdict: 'fail',
          citation: 'fields.activeNavClass',
          reason: `the current-section link is not visually marked active (class="${cls}")`,
        }
  })()

  out[2] = ((): ItemVerdict => {
    if (!nav) return { verdict: 'unsure', reason: 'no dashboard capture — no evidence' }
    const pos = asString(nav.fields?.navPosition)
    if (pos === undefined) {
      return { verdict: 'unsure', reason: 'no fields.navPosition — sticky state not captured' }
    }
    return pos === 'sticky'
      ? { verdict: 'pass', citation: 'fields.navPosition === sticky (computed style)' }
      : {
          verdict: 'fail',
          citation: 'fields.navPosition',
          reason: `the nav is not sticky (computed position="${pos}")`,
        }
  })()

  return out
}
