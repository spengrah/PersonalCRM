// Shared reader for the tour's fields.overdueCards projection — the rendered
// order of overdue cards with the DOM/CSS-only bits the aria tree cannot express
// (the urgency tier is a color class; the raw last_contacted is a relative-time
// string on the card). Recorded per sort-state capture (D2a). Used by CAD-026[1]
// (tier) and CAD-027 (order).

import { asArray, asRecord, asString } from '../evidence'
import type { Capture } from '../../../support/types'

export interface OverdueCard {
  name: string
  daysOverdue: number
  tierClass: string
  lastContacted: string | null
}

export function readOverdueCards(cap: Capture | undefined): OverdueCard[] | undefined {
  const raw = asArray(cap?.fields?.overdueCards)
  if (!raw) return undefined
  const out: OverdueCard[] = []
  for (const r of raw) {
    const rec = asRecord(r)
    if (!rec) continue
    const name = asString(rec.name)
    const daysOverdue = typeof rec.daysOverdue === 'number' ? rec.daysOverdue : undefined
    const tierClass = asString(rec.tierClass) ?? ''
    if (name === undefined || daysOverdue === undefined) continue
    out.push({
      name,
      daysOverdue,
      tierClass,
      lastContacted: asString(rec.lastContacted) ?? null,
    })
  }
  return out
}

// The urgency tier color the card SHOULD carry for a days-overdue value
// (dashboard/page.tsx getUrgencyColor boundaries: <=2 yellow, <=7 orange, else red).
export function expectedTierColor(daysOverdue: number): 'yellow' | 'orange' | 'red' {
  if (daysOverdue <= 2) return 'yellow'
  if (daysOverdue <= 7) return 'orange'
  return 'red'
}
