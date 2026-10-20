import type { ReviewRunResponse } from '@/types/generated/contact'

/** What the Threads tab says when a save fails. */
export const SAVE_ERROR = "Couldn't save. Try again."

/** A probability as a whole percentage. */
export function formatConfidence(confidence: number): string {
  return `${Math.round(confidence * 100)}%`
}

const month = new Intl.DateTimeFormat('en-US', { month: 'long', year: 'numeric', timeZone: 'UTC' })
const started = new Intl.DateTimeFormat('en-US', {
  month: 'short',
  day: 'numeric',
  hour: 'numeric',
  minute: '2-digit',
})

/**
 * A run as the run filter names it: "{method} · {month} · started {time}", the
 * month its window starts in and the local date and time it began, which tells
 * reruns of one month apart.
 */
export function runLabel(run: ReviewRunResponse): string {
  const from = month.format(new Date(run.from ?? run.started_at))
  return `${run.method_key} · ${from} · started ${started.format(new Date(run.started_at))}`
}
