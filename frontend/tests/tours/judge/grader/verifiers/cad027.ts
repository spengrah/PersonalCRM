// CAD-027 — the overdue list offers urgency, name, and recency orderings.
//   [0] urgency (default) orders most-overdue first
//   [1] name orders alphabetically
//   [2] last-contacted oldest first, never-contacted (null) last
//
// Each sort-state capture records fields.overdueCards in RENDERED order (D2a);
// the verifier checks the ordering invariant over that sequence.

import { byRole } from '../evidence'
import { readOverdueCards, type OverdueCard } from './overdue'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

function checkOrder(
  cards: OverdueCard[] | undefined,
  ok: (a: OverdueCard, b: OverdueCard) => boolean,
  citation: string,
  failReason: string
): ItemVerdict {
  if (!cards) return { verdict: 'unsure', reason: 'no fields.overdueCards — order not captured' }
  if (cards.length < 2) {
    return { verdict: 'unsure', reason: 'fewer than 2 overdue cards — ordering unprovable' }
  }
  for (let i = 1; i < cards.length; i++) {
    if (!ok(cards[i - 1], cards[i])) {
      return { verdict: 'fail', citation, reason: failReason }
    }
  }
  return { verdict: 'pass', citation }
}

export function cad027(set: CaptureSet): ItemVerdicts {
  const urgency = readOverdueCards(byRole(set, 'sort-urgency'))
  const name = readOverdueCards(byRole(set, 'sort-name'))
  const lastContacted = readOverdueCards(byRole(set, 'sort-last-contacted'))
  const out: ItemVerdicts = {}

  out[0] = checkOrder(
    urgency,
    (a, b) => a.daysOverdue >= b.daysOverdue,
    'urgency sort: days_overdue non-increasing',
    'urgency sort is not most-overdue-first'
  )

  out[1] = checkOrder(
    name,
    (a, b) => a.name.localeCompare(b.name) <= 0,
    'name sort: full_name alphabetical',
    'name sort is not alphabetical'
  )

  out[2] = checkOrder(
    lastContacted,
    (a, b) => {
      // never-contacted (null) sinks to the end; otherwise oldest (earliest)
      // first. Compare as instants (numeric), matching the app's Date.getTime().
      if (a.lastContacted === null) return b.lastContacted === null
      if (b.lastContacted === null) return true
      return new Date(a.lastContacted).getTime() <= new Date(b.lastContacted).getTime()
    },
    'last-contacted sort: oldest first, never-contacted last',
    'last-contacted sort is not oldest-first with never-contacted last'
  )

  return out
}
