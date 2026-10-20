'use client'

import { useEffect, useState } from 'react'
import { useRouter } from 'next/navigation'
import { ContactNavigationBar } from '@/components/contacts/contact-navigation-bar'
import { Navigation } from '@/components/layout/navigation'
import { threadAddress, threadsAddress } from '@/components/threads/address'
import { ReviewSwitch, useReview } from '@/components/threads/review-switch'
import { SaveFailures } from '@/components/threads/save-failures'
import {
  saveItem,
  useCloseThread,
  usePendingSaves,
  useReviewRuns,
  useThread,
} from '@/hooks/use-bench'
import { useKeyboardNavigation } from '@/hooks/use-keyboard-navigation'
import { THREAD_PAGE_SIZE, type ThreadListFilters } from '@/lib/bench-api'
import { THREAD_PAGE_COLUMN } from './column'
import { messageDomId } from './message'
import { PlainThread } from './plain-thread'
import { ReviewTimeline } from './review-timeline'
import { ThreadHeader } from './thread-header'

/** How long a message a link took the user to stays highlighted. */
const HIGHLIGHT_MS = 2500

const pageOf = (index: number) => Math.ceil(index / THREAD_PAGE_SIZE)

function LoadedThread({
  threadId,
  filters,
  runId,
}: {
  threadId: string
  /** The thread page's address: the list's filters and page. */
  filters: ThreadListFilters
  /** The run Review shows; absent with Review off. */
  runId: string | undefined
}) {
  const router = useRouter()
  const read = useThread(threadId, {
    type: filters.type,
    contact_id: filters.contact_id,
    run_id: runId,
  })
  const close = useCloseThread()
  const closePending = usePendingSaves()(saveItem.close(threadId))
  const [highlighted, setHighlighted] = useState<string | null>(null)
  useEffect(() => {
    if (!highlighted) return
    const timer = setTimeout(() => setHighlighted(null), HIGHLIGHT_MS)
    return () => clearTimeout(timer)
  }, [highlighted])

  const data = read.data
  const review = data?.review
  const position = data?.position
  const showMessage = (messageKey: string) => {
    document
      .getElementById(messageDomId(messageKey))
      ?.scrollIntoView({ block: 'center', behavior: 'smooth' })
    setHighlighted(messageKey)
  }
  // A close names the run and the thread; it is queued behind every save made
  // before it and never delays the page that follows.
  const closeThread = () => {
    if (review) close.mutate({ threadId, runId: review.run.id })
  }

  // The hook walks a list of ids; the server holds the list, so it is given
  // only this thread and its two neighbours. The pager's numbers come from the
  // server's position.
  const neighbours = [position?.previous_thread_id, threadId, position?.next_thread_id].filter(
    (id): id is string => Boolean(id)
  )
  const { canGoBack, canGoForward, goBack, goForward } = useKeyboardNavigation({
    ids: neighbours,
    currentId: threadId,
    enabled: Boolean(data),
    onNavigate: id => {
      const forward = id === position?.next_thread_id
      // Next closes the thread; Previous does not.
      if (forward) closeThread()
      const index = (position?.index ?? 1) + (forward ? 1 : -1)
      router.push(threadAddress(id, { ...filters, page: pageOf(index) }))
    },
  })

  const back = () =>
    router.push(
      threadsAddress({ ...filters, page: position ? pageOf(position.index) : filters.page })
    )

  return (
    <div className="min-h-screen bg-gray-50">
      <Navigation />
      {/* Sticks beneath the site navigation, which is 64px and a 1px border tall. */}
      <div data-thread-top className="sticky top-[65px] z-40">
        <ContactNavigationBar
          noun="thread"
          column={THREAD_PAGE_COLUMN}
          onBack={back}
          onPrevious={goBack}
          onNext={goForward}
          canGoBack={canGoBack}
          canGoForward={canGoForward}
          currentIndex={(position?.index ?? 1) - 1}
          totalCount={position?.total ?? 0}
          isLoading={!data}
          controls={<ReviewSwitch />}
        />
        {data && (
          <ThreadHeader
            thread={data.thread}
            review={review}
            onDone={closeThread}
            donePending={closePending}
          />
        )}
      </div>
      <SaveFailures current={threadId} />
      <main className={`mx-auto ${THREAD_PAGE_COLUMN} px-4 py-6 sm:px-6 lg:px-8`}>
        {read.isError ? (
          <p className="text-sm text-red-700">Failed to load the thread.</p>
        ) : !data ? (
          <p className="text-sm text-gray-500">Loading thread…</p>
        ) : review ? (
          <ReviewTimeline
            threadId={threadId}
            review={review}
            messages={data.messages}
            highlighted={highlighted}
            onShow={showMessage}
          />
        ) : (
          <PlainThread messages={data.messages} />
        )}
      </main>
    </div>
  )
}

/**
 * A thread's page: the contact page's navigation bar with the Review switch,
 * the thread's header, and its messages: with Review off in order; with Review
 * on as the chosen run's nights. The run is the address's, else the newest the
 * runs list holds. Moving to the next thread closes this one; going back,
 * previous or leaving does not.
 */
export function ThreadPage({
  threadId,
  filters,
}: {
  threadId: string
  filters: ThreadListFilters
}) {
  const [review] = useReview()
  const runs = useReviewRuns()
  const runId = review ? (filters.run_id ?? runs.data?.runs[0]?.id) : undefined
  const waitingForRuns = review && !filters.run_id && runs.isPending
  if (waitingForRuns) {
    return (
      <div className="min-h-screen bg-gray-50">
        <Navigation />
        <p className="p-6 text-sm text-gray-500">Loading thread…</p>
      </div>
    )
  }
  return <LoadedThread key={threadId} threadId={threadId} filters={filters} runId={runId} />
}
