// CAD-026 — the dashboard is an action-required list of overdue contacts.
//   [0] overdue cards + count in the Action Required header
//   [1] each card shows tier/cadence/recency/methods/action (tier via fields, D2a)
//   [2] nothing overdue → all-caught-up state (route-empty capture)

import { ariaTextIncludes, byRole, findAllAria } from '../evidence'
import { expectedTierColor, readOverdueCards } from './overdue'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

export function cad026(set: CaptureSet): ItemVerdicts {
  const overdue = byRole(set, 'sort-urgency')
  const caughtUp = byRole(set, 'caught-up')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    if (!overdue) return { verdict: 'unsure', reason: 'no overdue capture — no evidence' }
    const header = ariaTextIncludes(overdue.aria, 'Action Required')
    const cardCount = findAllAria(
      overdue.aria,
      n => n.role === 'button' && n.name === 'Mark as Contacted'
    ).length
    if (header && cardCount > 0) {
      return {
        verdict: 'pass',
        citation: `Action Required header + ${cardCount} overdue card(s)`,
      }
    }
    return {
      verdict: 'fail',
      citation: 'overdue capture aria',
      reason: `expected the Action Required header with overdue cards (header ${header ? 'present' : 'absent'}, cards=${cardCount})`,
    }
  })()

  out[1] = ((): ItemVerdict => {
    const cards = readOverdueCards(overdue)
    if (!cards) {
      return { verdict: 'unsure', reason: 'no fields.overdueCards — tier evidence not captured' }
    }
    if (cards.length === 0) {
      return { verdict: 'unsure', reason: 'no overdue cards recorded — nothing to check' }
    }
    for (const c of cards) {
      const expected = expectedTierColor(c.daysOverdue)
      if (!c.tierClass.includes(expected)) {
        return {
          verdict: 'fail',
          citation: `card '${c.name}' tierClass`,
          reason: `days_overdue=${c.daysOverdue} should carry the ${expected} urgency tier, got class="${c.tierClass}"`,
        }
      }
    }
    return {
      verdict: 'pass',
      citation: 'every overdue card carries the urgency tier matching its days-overdue boundary',
    }
  })()

  out[2] = ((): ItemVerdict => {
    if (!caughtUp) return { verdict: 'unsure', reason: 'no caught-up capture — no evidence' }
    return ariaTextIncludes(caughtUp.aria, 'All caught up')
      ? { verdict: 'pass', citation: "route-empty overdue → 'All caught up' state" }
      : {
          verdict: 'fail',
          citation: 'caught-up capture aria',
          reason: 'nothing-overdue did not render the all-caught-up state',
        }
  })()

  return out
}
