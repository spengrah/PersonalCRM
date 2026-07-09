// DSH-007 — search is the contact list's; the dashboard exposes no global search.
//   [0] contact text search via the contact-list search input
//   [1] no dashboard/global search surface (no top-bar search box / palette)

import { byRole, findAllAria, findByRoleName } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'
import type { AriaNode } from '../../../support/types'

// Any search affordance: a searchbox, or a text input whose name reads "Search…".
function searchSurfaces(aria: AriaNode): AriaNode[] {
  return findAllAria(
    aria,
    n => n.role === 'searchbox' || (n.role === 'textbox' && /search/i.test(n.name ?? ''))
  )
}

export function dsh007(set: CaptureSet): ItemVerdicts {
  const dashboard = byRole(set, 'dashboard')
  const contactsSearch = byRole(set, 'contacts-search')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    if (!contactsSearch) return { verdict: 'unsure', reason: 'no /contacts capture — no evidence' }
    const input =
      findByRoleName(contactsSearch.aria, 'textbox', /Search contacts/i) ??
      findByRoleName(contactsSearch.aria, 'searchbox', /Search contacts/i)
    return input
      ? { verdict: 'pass', citation: "contact-list 'Search contacts...' input" }
      : {
          verdict: 'fail',
          citation: '/contacts aria',
          reason: 'the contact list has no text-search input',
        }
  })()

  out[1] = ((): ItemVerdict => {
    if (!dashboard) return { verdict: 'unsure', reason: 'no dashboard capture — no evidence' }
    const surfaces = searchSurfaces(dashboard.aria)
    return surfaces.length === 0
      ? { verdict: 'pass', citation: 'dashboard aria has no searchbox / search input' }
      : {
          verdict: 'fail',
          citation: 'dashboard aria',
          reason: `the dashboard exposes a search surface (${surfaces.map(s => s.name ?? s.role).join(', ')})`,
        }
  })()

  return out
}
