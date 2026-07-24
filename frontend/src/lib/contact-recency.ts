import { formatRelativeTime } from './utils'

/**
 * The fields the overdue card's recency line reads. `last_contacted` is unset until
 * a two-way connection happens (CON-001 leaves it NULL at creation; CAD-006 moves it
 * only on inbound/mutual), so its absence means "never connected" — not "unknown".
 */
export interface RecencySource {
  created_at: string
  last_contacted?: string
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
 * hardcodes one label around a formatted value cannot say both. The relative phrase
 * itself comes from formatRelativeTime, the shared singular-aware formatter, so the
 * card never renders "1 weeks ago".
 */
export function formatOverdueRecency(contact: RecencySource, currentTime: Date): string {
  const source = contact.last_contacted ?? contact.created_at
  const relative = formatRelativeTime(source, currentTime)
  if (!relative) return ''

  return contact.last_contacted ? `Last connected ${relative}` : `Added ${relative}`
}
