import { clsx } from 'clsx'
import type { ThreadStatus } from '@/types/generated/judgment'

const LOOK: Record<ThreadStatus, string> = {
  to_review: 'border-amber-300 bg-amber-50 text-amber-800',
  nothing_flagged: 'border-gray-300 bg-gray-100 text-gray-700',
  reviewed: 'border-green-300 bg-green-50 text-green-800',
}

/** A thread's status as the server computed it: its pill reads "{n} to review", "Nothing flagged" or "Reviewed". */
export function StatusPill({ status, toReview }: { status: ThreadStatus; toReview: number }) {
  const text = {
    to_review: `${toReview} to review`,
    nothing_flagged: 'Nothing flagged',
    reviewed: 'Reviewed',
  }[status]
  return (
    <span
      data-thread-status={status}
      className={clsx(
        'inline-flex whitespace-nowrap rounded-full border px-2 py-0.5 text-xs font-medium',
        LOOK[status]
      )}
    >
      {text}
    </span>
  )
}
