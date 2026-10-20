import type { ReviewRunResponse } from '@/types/generated/contact'

/** What the Threads tab says when a save fails. */
export const SAVE_ERROR = "Couldn't save. Try again."

/** A probability as a whole percentage. */
export function formatConfidence(confidence: number): string {
  return `${Math.round(confidence * 100)}%`
}

const month = new Intl.DateTimeFormat('en-US', { month: 'long', year: 'numeric', timeZone: 'UTC' })

/** A run as the run filter names it: "{method} · {month}", the month its window starts in. */
export function runLabel(run: ReviewRunResponse): string {
  return `${run.method_key} · ${month.format(new Date(run.from ?? run.started_at))}`
}
