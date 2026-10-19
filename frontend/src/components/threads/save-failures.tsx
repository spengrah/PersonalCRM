'use client'

import Link from 'next/link'
import { useFailedSaves } from '@/hooks/use-bench'
import { threadAddress } from './address'

/**
 * The saves that failed and still wait for the user, kept by the save queue
 * after the page they came from is gone. `except` leaves out a thread the page
 * shows, whose controls show their own failures.
 */
export function SaveFailures({ except }: { except?: string }) {
  const failures = useFailedSaves().filter(failure => failure.threadId !== except)
  if (failures.length === 0) return null
  return (
    <div data-save-failures className="space-y-2">
      {failures.map(failure => (
        <div
          key={failure.id}
          role="alert"
          className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-red-200 bg-red-50 px-4 py-2 text-sm text-red-800 sm:px-6"
        >
          <span>
            {`Couldn't save ${failure.what} on a thread you left.`}
            {failure.closeHeld && ' Its close waits until this is saved.'}
          </span>
          <Link
            href={threadAddress(failure.threadId, { run_id: failure.runId })}
            className="font-medium text-blue-600 hover:text-blue-800 hover:underline"
          >
            Open thread
          </Link>
          <button
            type="button"
            onClick={failure.retry}
            className="font-medium text-blue-600 hover:text-blue-800 hover:underline"
          >
            Retry
          </button>
          <button
            type="button"
            onClick={failure.discard}
            className="font-medium text-gray-700 hover:text-gray-900 hover:underline"
          >
            Discard
          </button>
        </div>
      ))}
    </div>
  )
}
