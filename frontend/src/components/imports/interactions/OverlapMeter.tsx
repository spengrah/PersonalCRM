'use client'

import type { NeedsAttentionAttendee } from '@/types/import'

interface OverlapMeterProps {
  attendees: NeedsAttentionAttendee[]
  /** Authoritative shared count (from the detection-time snapshot). Drives
   * the "N shared" label — NOT the count of matched pips. */
  overlapCount: number
}

/**
 * One pip per attendee — filled blue when that attendee is in the
 * session's implied set, gray otherwise. The "N shared" label is driven
 * by `overlapCount` (the authoritative detection-time count), which is
 * decoupled from the per-pip fill (best-effort name emphasis).
 */
export function OverlapMeter({ attendees, overlapCount }: OverlapMeterProps) {
  return (
    <div className="flex items-center gap-1.5">
      <div className="flex items-center gap-1" aria-hidden="true">
        {attendees.map((a, i) => (
          <span
            key={i}
            title={a.name}
            className={`h-[7px] w-[7px] rounded-full ${a.matched ? 'bg-blue-600' : 'bg-gray-300'}`}
          />
        ))}
      </div>
      <span
        className={`text-xs font-medium ${overlapCount > 0 ? 'text-blue-700' : 'text-gray-500'}`}
      >
        {overlapCount} shared
      </span>
    </div>
  )
}
