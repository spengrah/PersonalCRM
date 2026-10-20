const day = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' })
const dayOfMonth = new Intl.DateTimeFormat('en-US', { day: 'numeric' })

/**
 * A date as the thread page writes it ("Jan 3"), and a span of dates ("Jan 2–3",
 * or "Jan 31–Feb 2" across months).
 */
export function formatDay(iso: string): string {
  return day.format(new Date(iso))
}

export function formatDays(firstIso: string, lastIso: string): string {
  const first = new Date(firstIso)
  const last = new Date(lastIso)
  if (day.format(first) === day.format(last)) return day.format(first)
  if (first.getMonth() === last.getMonth() && first.getFullYear() === last.getFullYear()) {
    return `${day.format(first)}–${dayOfMonth.format(last)}`
  }
  return `${day.format(first)}–${day.format(last)}`
}

/** "{n} new message(s)" and the like: a count and its noun, in its right number. */
export function count(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? '' : 's'}`
}
