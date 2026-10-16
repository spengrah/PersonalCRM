import { CHANNEL_LABELS, formatDateTime } from '@/components/timeline'
import type { JudgmentRunResponse } from '@/types/generated/contact'
import { INCOMPLETE_REASON_LABELS, formatWindow, runKindLabel } from './format'

/** The run a view is narrowed to: what it judged, and the threads it could not. */
export function RunSummary({ run }: { run: JudgmentRunResponse }) {
  return (
    <section aria-label="Run summary" className="border-b border-gray-200">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 bg-blue-50 px-4 py-3 text-sm text-blue-900 sm:px-6">
        <span className="font-medium">{runKindLabel(run.kind)}</span>
        <span>{formatWindow(run)}</span>
        <span>
          {run.method_key} {run.method_version}
        </span>
        <span>{run.finished_at ? `Finished ${formatDateTime(run.finished_at)}` : 'Running'}</span>
        <span>
          {run.judged_count} thread{run.judged_count === 1 ? '' : 's'} judged
        </span>
        {run.incomplete.length > 0 && (
          <span className="text-amber-800">{run.incomplete.length} incomplete</span>
        )}
      </div>
      {run.incomplete.length > 0 && (
        <details open className="px-4 py-3 text-sm sm:px-6">
          <summary className="cursor-pointer font-medium text-gray-900">Incomplete threads</summary>
          <ul aria-label="Incomplete threads" className="mt-2 space-y-1">
            {run.incomplete.map(thread => (
              <li
                key={`${thread.thread_id}-${thread.cut_off}`}
                data-incomplete-thread={thread.thread_id}
                className="flex flex-wrap gap-x-3 text-gray-700"
              >
                <span className="font-medium text-gray-900">
                  {thread.title || CHANNEL_LABELS[thread.venue_kind] || thread.venue_kind}
                </span>
                <span>cut-off {formatDateTime(thread.cut_off)}</span>
                <span className="text-amber-800">{INCOMPLETE_REASON_LABELS[thread.reason]}</span>
              </li>
            ))}
          </ul>
        </details>
      )}
    </section>
  )
}
