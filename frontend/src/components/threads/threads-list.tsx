'use client'

import Link from 'next/link'
import { clsx } from 'clsx'
import { ChevronRight } from 'lucide-react'
import { formatDateTime } from '@/components/timeline'
import { Pagination } from '@/components/ui/pagination'
import { Select } from '@/components/ui/select'
import { useReviewRuns, useThreads } from '@/hooks/use-bench'
import type { ThreadListFilters } from '@/lib/bench-api'
import type { ThreadRowResponse, ThreadRunSummaryResponse } from '@/types/generated/contact'
import { threadAddress } from './address'
import { ContactFilter } from './contact-filter'
import { runLabel } from './format'
import { useReview } from './review-switch'
import { StatusPill } from './status-pill'
import { GroupChip, VENUE_LABELS, VenueChip } from './venue'

interface ThreadsListProps {
  /** The tab's address. */
  filters: ThreadListFilters
  onChange: (next: ThreadListFilters) => void
}

const ROW_COLUMNS = 'lg:grid lg:grid-cols-[170px_minmax(0,1fr)_168px_150px_130px_40px] lg:gap-4'

function Count({ thread, review }: { thread: ThreadRowResponse; review: boolean }) {
  if (review && thread.review) return <>{`${thread.review.judged_count} judged in this run`}</>
  const n = thread.message_count
  if (thread.venue === 'calendar') return <>{`${n} meeting note${n === 1 ? '' : 's'}`}</>
  return <>{`${n.toLocaleString('en-US')} message${n === 1 ? '' : 's'}`}</>
}

function SummaryLine({ summary }: { summary: ThreadRunSummaryResponse }) {
  return (
    <div
      data-run-summary
      className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 border-b border-gray-200 bg-gray-50 px-4 py-2 text-sm text-gray-700 sm:px-6"
    >
      <span>
        {summary.judged} threads judged in this run · {summary.to_review} to review ·{' '}
        {summary.nothing_flagged} with nothing flagged · {summary.reviewed} reviewed
      </span>
      <span className="text-xs text-gray-500">Newest message first, as with Review off</span>
    </div>
  )
}

function ThreadRow({
  thread,
  review,
  open,
}: {
  thread: ThreadRowResponse
  review: boolean
  /** The address of the thread's page. */
  open: string
}) {
  return (
    <article
      role="listitem"
      data-thread-id={thread.thread_id}
      className={clsx('flex flex-col gap-2 px-4 py-4 sm:px-6', ROW_COLUMNS, 'lg:items-start')}
    >
      <div className="flex flex-wrap items-center gap-2">
        <VenueChip venue={thread.venue} />
        {thread.is_group && <GroupChip />}
      </div>
      <div className="min-w-0">
        {thread.title && (
          <div data-thread-title className="truncate text-sm font-semibold text-gray-900">
            {thread.title}
          </div>
        )}
        <div data-participants className="text-sm">
          {thread.participants.map((participant, index) => (
            <span key={participant.contact_id}>
              {index > 0 && ', '}
              <Link
                href={`/contacts/${participant.contact_id}`}
                className="text-blue-600 hover:text-blue-800 hover:underline"
              >
                {participant.name}
              </Link>
            </span>
          ))}
        </div>
      </div>
      <time dateTime={thread.latest_at} className="text-sm text-gray-500">
        {formatDateTime(thread.latest_at)}
      </time>
      <div data-count className="text-sm text-gray-500">
        <Count thread={thread} review={review} />
      </div>
      <div>
        {review && thread.review && (
          <StatusPill status={thread.review.status} toReview={thread.review.to_review} />
        )}
      </div>
      <Link
        href={open}
        aria-label="Open thread"
        className="flex h-8 w-8 items-center justify-center rounded-md text-gray-400 hover:bg-gray-100 hover:text-gray-600"
      >
        <ChevronRight aria-hidden="true" className="h-5 w-5" />
      </Link>
    </article>
  )
}

/**
 * The Threads tab: one row per thread, newest latest message first, whether
 * Review is on or off. With Review on, a Run filter narrows the list to the
 * threads a run judged, holding the newest run when the address names none,
 * and each row adds its judged count and the status the server computed. With
 * Review off the address's run is ignored and no judgment shows.
 */
export function ThreadsList({ filters, onChange }: ThreadsListProps) {
  const [review] = useReview()
  const runs = useReviewRuns()
  const runList = runs.data?.runs ?? []
  const runId = review ? (filters.run_id ?? runList[0]?.id) : undefined
  const waitingForRuns = review && !filters.run_id && runs.isPending
  const list = useThreads({ ...filters, run_id: runId }, !waitingForRuns)
  const threads = list.data?.data?.items ?? []
  const summary = list.data?.data?.summary
  const pagination = list.data?.meta?.pagination
  const set = (patch: Partial<ThreadListFilters>) =>
    onChange({ ...filters, ...patch, page: undefined })
  const filtered = Boolean(filters.venue || filters.contact_id || (review && filters.run_id))

  return (
    <>
      <div className="flex flex-wrap items-end gap-4 border-b border-gray-200 px-4 py-4 sm:px-6">
        {review && (
          <div className="min-w-[12rem] flex-1">
            <Select
              id="threads-run"
              label="Run"
              value={runId ?? ''}
              onChange={event => set({ run_id: event.target.value || undefined })}
            >
              {runList.map(run => (
                <option key={run.id} value={run.id}>
                  {runLabel(run)}
                </option>
              ))}
            </Select>
          </div>
        )}
        <div className="flex flex-col gap-1 text-sm text-gray-700">
          <span id="threads-venue-label">Venue</span>
          <div
            role="group"
            aria-labelledby="threads-venue-label"
            className="inline-flex flex-wrap overflow-hidden rounded-md border border-gray-300"
          >
            {[
              { key: undefined, label: 'All' },
              ...Object.entries(VENUE_LABELS).map(([key, label]) => ({
                key: key as keyof typeof VENUE_LABELS,
                label,
              })),
            ].map(option => (
              <button
                key={option.key ?? 'all'}
                type="button"
                aria-pressed={filters.venue === option.key}
                onClick={() => set({ venue: option.key })}
                className={clsx(
                  'border-l border-gray-300 px-3 py-1 text-sm first:border-l-0',
                  filters.venue === option.key
                    ? 'bg-gray-900 text-white'
                    : 'bg-white text-gray-600 hover:bg-gray-50'
                )}
              >
                {option.label}
              </button>
            ))}
          </div>
        </div>
        <ContactFilter
          contactId={filters.contact_id}
          onChange={contactId => set({ contact_id: contactId })}
        />
        {filtered && (
          <button
            type="button"
            onClick={() => onChange({})}
            className="pb-1 text-sm text-blue-600 hover:text-blue-800 hover:underline"
          >
            Clear filters
          </button>
        )}
      </div>
      {review && summary && <SummaryLine summary={summary} />}
      {list.isError ? (
        <p className="p-6 text-sm text-red-700">Failed to load threads.</p>
      ) : list.isPending ? (
        <p className="p-6 text-sm text-gray-500">Loading threads…</p>
      ) : threads.length === 0 ? (
        <p className="p-6 text-sm text-gray-500">
          {runId ? 'This run judged no threads' : 'No threads match these filters'}
        </p>
      ) : (
        <div role="list" aria-label="Threads" className="divide-y divide-gray-200">
          {threads.map(thread => (
            <ThreadRow
              key={thread.thread_id}
              thread={thread}
              review={review}
              open={threadAddress(thread.thread_id, {
                ...filters,
                run_id: review ? runId : filters.run_id,
              })}
            />
          ))}
        </div>
      )}
      {pagination && pagination.pages > 1 && (
        <div className="border-t border-gray-200 px-4 py-4 sm:px-6">
          <Pagination
            page={pagination.page}
            pages={pagination.pages}
            total={pagination.total}
            noun="threads"
            onPageChange={page => onChange({ ...filters, page })}
          />
        </div>
      )}
    </>
  )
}
