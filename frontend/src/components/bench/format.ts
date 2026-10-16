import type { IncompleteReason, RunKind } from '@/types/generated/judgment'
import type { JudgmentRunResponse } from '@/types/generated/contact'

const RUN_KIND_LABELS: Record<RunKind, string> = {
  scheduled: 'Scheduled',
  historical: 'Historical',
  rerun: 'Rerun',
}

export const INCOMPLETE_REASON_LABELS: Record<IncompleteReason, string> = {
  over_limit: "Over the model's limit",
  call_failed: 'The call failed',
  input_changed: 'Content changed during the run',
}

/** A calendar date (YYYY-MM-DD) as "Fri, Jan 16". */
export function formatDueDate(date: string): string {
  return new Intl.DateTimeFormat('en-US', {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    timeZone: 'UTC',
  }).format(new Date(`${date}T00:00:00Z`))
}

/** A probability as a whole percentage. */
export function formatConfidence(confidence: number): string {
  return `${Math.round(confidence * 100)}%`
}

const monthDay = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' })
const dayOnly = new Intl.DateTimeFormat('en-US', { day: 'numeric' })
const year = new Intl.DateTimeFormat('en-US', { year: 'numeric' })

/** A run's content window, as "Jan 1–31, 2026"; "No window" when it has none. */
export function formatWindow(run: JudgmentRunResponse): string {
  if (!run.from || !run.to) return 'No window'
  const from = new Date(run.from)
  const to = new Date(run.to)
  const sameMonth = from.getMonth() === to.getMonth() && from.getFullYear() === to.getFullYear()
  const end = sameMonth ? dayOnly.format(to) : monthDay.format(to)
  return `${monthDay.format(from)}–${end}, ${year.format(to)}`
}

/** A run as the run filter names it. */
export function runLabel(run: JudgmentRunResponse): string {
  return `${RUN_KIND_LABELS[run.kind]} · ${formatWindow(run)} · ${run.method_key} ${run.method_version}`
}

export function runKindLabel(kind: RunKind): string {
  return RUN_KIND_LABELS[kind]
}
