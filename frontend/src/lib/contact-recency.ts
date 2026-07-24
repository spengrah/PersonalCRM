import { getLocalCalendarDayDifference } from './utils'

/**
 * The fields the overdue card's recency line reads. `last_contacted` is unset until
 * a two-way connection happens (CON-001 leaves it NULL at creation; CAD-006 moves it
 * only on inbound/mutual), so its absence means "never connected" — not "unknown".
 */
export interface RecencySource {
  created_at: string
  last_contacted?: string
}

function relativeDay(date: Date, referenceDate: Date): string {
  const diffDays = getLocalCalendarDayDifference(date, referenceDate)

  if (diffDays < 0) {
    const futureDays = Math.abs(diffDays)
    return futureDays === 1 ? 'in 1 day' : `in ${futureDays} days`
  }
  if (diffDays === 0) return 'today'
  if (diffDays === 1) return 'yesterday'
  if (diffDays <= 7) return `${diffDays} days ago`
  if (diffDays <= 30) return `${Math.floor(diffDays / 7)} weeks ago`
  return `${Math.floor(diffDays / 30)} months ago`
}

/**
 * The cadence base date as epoch ms: last_contacted when set, else created_at — the
 * same base contact_by is computed from (CAD-002[0]). Sorting on it keeps
 * never-connected contacts in the recency ordering, ranked by how long they have
 * waited, instead of sinking them below every contact that has a connection.
 */
export function cadenceBaseDate(contact: RecencySource): number {
  return new Date(contact.last_contacted ?? contact.created_at).getTime()
}

/**
 * Builds the overdue card's recency line as a COMPLETE phrase, label included. The
 * label and the value have to be decided together: a contact with no connection has
 * no "last contacted" date to report, only a date it was added, and a template that
 * hardcodes one label around a formatted value cannot say both.
 */
export function formatOverdueRecency(contact: RecencySource, currentTime: Date): string {
  const date = new Date(contact.last_contacted ?? contact.created_at)
  if (Number.isNaN(date.getTime())) return ''

  return contact.last_contacted
    ? `Last connected ${relativeDay(date, currentTime)}`
    : `Added ${relativeDay(date, currentTime)}`
}
