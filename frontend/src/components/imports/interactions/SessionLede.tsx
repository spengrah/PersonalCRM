'use client'

import { MessageCircle, Clock } from 'lucide-react'

interface SessionLedeProps {
  title: string | null
  meetingAt: string
  summaryExcerpt?: string | null
}

/**
 * The session header block shared by conflict + orphan cards: a message
 * icon, the session title, an "Anarlog session" badge, the meeting time,
 * and a quoted summary excerpt. Middle fidelity — NO implied-participant
 * chips and NO duration (deferred per spec).
 */
export function SessionLede({ title, meetingAt, summaryExcerpt }: SessionLedeProps) {
  return (
    <div className="flex items-start gap-3.5">
      <div className="flex h-10 w-10 flex-shrink-0 items-center justify-center rounded-full bg-gray-100 text-gray-500">
        <MessageCircle className="h-5 w-5" />
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-base font-semibold text-gray-900">{title || 'Untitled session'}</h3>
          <span className="inline-flex items-center rounded-full bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-600">
            Anarlog session
          </span>
        </div>
        <div className="mt-1 flex items-center gap-1.5 text-[0.8125rem] text-gray-500">
          <Clock className="h-3.5 w-3.5 text-gray-400" />
          <span>{formatMeetingTime(meetingAt)}</span>
        </div>
        {summaryExcerpt && (
          <p className="mt-2.5 border-l-2 border-gray-200 pl-3 text-sm leading-relaxed text-gray-600">
            {summaryExcerpt}
          </p>
        )}
      </div>
    </div>
  )
}

/** Format an ISO timestamp as a readable local date + time. */
function formatMeetingTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString(undefined, {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  })
}
