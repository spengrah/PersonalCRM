// CAD-026 — the dashboard is an action-required list of overdue contacts.
//   [0] overdue cards + the count in the header
//   [1] each card shows urgency tier, cadence, recency, reachable methods, action
//   [2] nothing overdue → all-caught-up state (route-empty capture)
//
// [1] grades the urgency TIER per-card from fields.overdueCards[].tierClass (the
// tier is a CSS color the aria cannot express, D2a) and the other four card
// sub-elements (cadence, recency, a contact method, the suggested action) from
// the card aria. The aria sub-elements are graded at CARD-TEMPLATE level (>= 1
// present across the captured cards) rather than per-card, because ariaCap
// truncates deep card nodes — recorded as a caveat on the classification row.

import { ariaTextIncludes, byRole, findAllAria } from '../evidence'
import { expectedTierColor, readOverdueCards } from './overdue'
import type { AriaNode, Capture } from '../../../support/types'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

const METHOD_LABEL = /^(Email|Phone|Telegram|Signal|Discord|Twitter|WhatsApp|GChat)$/

function ariaHasText(aria: AriaNode, re: RegExp): boolean {
  return findAllAria(aria, n => re.test(n.text ?? '') || re.test(n.name ?? '')).length > 0
}

// Card headings (contact names) are the most-retained per-card node under ariaCap
// truncation; count them as the visible-card count.
function overdueCardCount(cap: Capture): number {
  return findAllAria(cap.aria, n => n.role === 'heading' && n.level === 3).length
}

export function cad026(set: CaptureSet): ItemVerdicts {
  const overdue = byRole(set, 'sort-urgency')
  const caughtUp = byRole(set, 'caught-up')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    if (!overdue) return { verdict: 'unsure', reason: 'no overdue capture — no evidence' }
    const header = ariaTextIncludes(overdue.aria, 'Action Required')
    const countNode = findAllAria(overdue.aria, n =>
      /^\d+\s+contacts?\s+need\s+your\s+attention/.test(n.text ?? '')
    )[0]
    const headerCount = countNode ? Number(countNode.text?.match(/^(\d+)/)?.[1]) : undefined
    const cards = overdueCardCount(overdue)
    if (!header || cards === 0) {
      return {
        verdict: 'fail',
        citation: 'overdue capture aria',
        reason: `expected the Action Required header with overdue cards (header ${header ? 'present' : 'absent'}, cards=${cards})`,
      }
    }
    if (headerCount === undefined || Number.isNaN(headerCount)) {
      return {
        verdict: 'fail',
        citation: 'Action Required header',
        reason: 'the header does not state a numeric overdue count',
      }
    }
    // The capture truncates the card list (ariaCap), so the header count must be
    // AT LEAST the visible-card count, not exactly equal.
    if (headerCount < cards) {
      return {
        verdict: 'fail',
        citation: 'Action Required header count',
        reason: `the header count (${headerCount}) is below the number of rendered cards (${cards})`,
      }
    }
    return {
      verdict: 'pass',
      citation: `Action Required header count=${headerCount} (>= ${cards} rendered card(s))`,
    }
  })()

  out[1] = ((): ItemVerdict => {
    const cards = readOverdueCards(overdue)
    if (!cards || !overdue) {
      return { verdict: 'unsure', reason: 'no fields.overdueCards — tier evidence not captured' }
    }
    if (cards.length === 0) {
      return { verdict: 'unsure', reason: 'no overdue cards recorded — nothing to check' }
    }
    // Urgency tier: per-card, from the CSS color class the aria cannot express.
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
    // The remaining four sub-elements: present in the card aria (template-level).
    const missing: string[] = []
    if (!ariaHasText(overdue.aria, /\(\w+ cadence\)/)) missing.push('cadence')
    if (
      !(
        ariaHasText(overdue.aria, /days? overdue/) &&
        ariaHasText(overdue.aria, /Last contacted|Never contacted/)
      )
    ) {
      missing.push('recency (days-overdue + last-contact)')
    }
    if (findAllAria(overdue.aria, n => METHOD_LABEL.test(n.text ?? '')).length === 0) {
      missing.push('a reachable contact method')
    }
    if (!ariaHasText(overdue.aria, /💡/)) missing.push('the suggested action')
    if (missing.length > 0) {
      return {
        verdict: 'fail',
        citation: 'overdue card aria',
        reason: `the overdue card is missing: ${missing.join(', ')}`,
      }
    }
    return {
      verdict: 'pass',
      citation:
        'every card carries the urgency tier matching its days-overdue boundary; the card aria shows cadence, recency, a contact method, and the suggested action',
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
