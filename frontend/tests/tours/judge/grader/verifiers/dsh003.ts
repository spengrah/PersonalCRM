// DSH-003 — the dashboard always offers a path to add or browse contacts.
//   [0] an add-contact action is always available from the header
//   [1] the caught-up state additionally offers add + view-list affordances

import { byRole, findByRoleName } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'
import type { AriaNode } from '../../../support/types'

// An add-contact / view-list affordance is a link OR a button carrying the name
// (the header renders a Link wrapping a Button; EmptyState renders Links).
function hasControl(aria: AriaNode, name: string): boolean {
  return (
    findByRoleName(aria, 'link', name) !== undefined ||
    findByRoleName(aria, 'button', name) !== undefined
  )
}

export function dsh003(set: CaptureSet): ItemVerdicts {
  const dashboard = byRole(set, 'dashboard')
  const caughtUp = byRole(set, 'caught-up')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    if (!dashboard) return { verdict: 'unsure', reason: 'no dashboard capture — no evidence' }
    return hasControl(dashboard.aria, 'Add Contact')
      ? { verdict: 'pass', citation: "header 'Add Contact' affordance" }
      : {
          verdict: 'fail',
          citation: 'dashboard header',
          reason: 'the header has no add-contact affordance',
        }
  })()

  out[1] = ((): ItemVerdict => {
    if (!caughtUp) return { verdict: 'unsure', reason: 'no caught-up capture — no evidence' }
    const viewList = hasControl(caughtUp.aria, 'View All Contacts')
    const addNew = hasControl(caughtUp.aria, 'Add New Contact')
    if (viewList && addNew) {
      return { verdict: 'pass', citation: "caught-up 'View All Contacts' + 'Add New Contact'" }
    }
    return {
      verdict: 'fail',
      citation: 'caught-up affordances',
      reason: `the caught-up state is missing ${!viewList ? 'View All Contacts' : ''}${!viewList && !addNew ? ' + ' : ''}${!addNew ? 'Add New Contact' : ''}`,
    }
  })()

  return out
}
