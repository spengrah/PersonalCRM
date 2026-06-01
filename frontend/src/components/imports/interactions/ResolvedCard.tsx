'use client'

import { CheckCircle } from 'lucide-react'

interface ResolvedCardProps {
  title: string | null
  /** When set, the session was linked to this target's label; otherwise it
   * was logged as an impromptu session. */
  linkedTo?: string
}

/**
 * Brief green confirmation shown in place of a resolved card before the
 * row leaves the queue.
 */
export function ResolvedCard({ title, linkedTo }: ResolvedCardProps) {
  return (
    <div className="rounded-lg border border-gray-200 border-l-2 border-l-green-600 bg-green-50 p-4">
      <div className="flex items-center gap-3">
        <CheckCircle className="h-5 w-5 flex-shrink-0 text-green-600" />
        <div className="text-sm text-green-800">
          <span className="font-semibold">{title || 'Session'}</span>{' '}
          {linkedTo ? (
            <>
              linked to <span className="font-semibold">{linkedTo}</span>. Interactions reconciled.
            </>
          ) : (
            'logged as an impromptu session.'
          )}
        </div>
      </div>
    </div>
  )
}
