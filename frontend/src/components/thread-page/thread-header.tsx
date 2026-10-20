'use client'

import Link from 'next/link'
import { Button } from '@/components/ui/button'
import { runLabel } from '@/components/threads/format'
import { StatusPill } from '@/components/threads/status-pill'
import { GroupChip, VenueChip } from '@/components/threads/venue'
import { THREAD_PAGE_COLUMN } from './column'
import type { ThreadReviewResponse, ThreadRowResponse } from '@/types/generated/contact'

interface ThreadHeaderProps {
  thread: ThreadRowResponse
  /** The run's review of the thread; absent with Review off. */
  review?: ThreadReviewResponse
  /** Closes the thread; Done is offered only with a review. */
  onDone: () => void
  donePending: boolean
}

/**
 * The thread page's header: the thread's title, venue, Group chip and
 * participants, each participant a link. With a review it adds the thread's
 * status, the run and Done.
 */
export function ThreadHeader({ thread, review, onDone, donePending }: ThreadHeaderProps) {
  return (
    <header data-thread-header className="border-b border-gray-200 bg-white">
      <div
        className={`mx-auto flex ${THREAD_PAGE_COLUMN} items-start justify-between gap-4 px-4 py-3 sm:px-6 lg:px-8`}
      >
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            {thread.title && (
              <h2 data-thread-title className="text-[22px] font-bold leading-tight text-gray-900">
                {thread.title}
              </h2>
            )}
            <VenueChip venue={thread.venue} />
            {thread.is_group && <GroupChip />}
          </div>
          <div className="flex flex-wrap items-center gap-x-1 text-sm text-gray-600">
            <span data-participants>
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
            </span>
            {review && (
              <>
                <span aria-hidden="true">·</span>
                <StatusPill status={review.status} toReview={review.to_review} />
                <span data-run>{`· ${runLabel(review.run)} run`}</span>
              </>
            )}
          </div>
        </div>
        {review && (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="shrink-0"
            disabled={donePending}
            onClick={onDone}
          >
            Done
          </Button>
        )}
      </div>
    </header>
  )
}
