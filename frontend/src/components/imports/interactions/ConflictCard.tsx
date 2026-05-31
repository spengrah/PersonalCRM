'use client'

import { Calendar, Phone, Link2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { SessionLede } from './SessionLede'
import { OverlapMeter } from './OverlapMeter'
import type { NeedsAttentionItem, NeedsAttentionCandidate } from '@/types/import'

interface ConflictCardProps {
  item: NeedsAttentionItem
  /** Disable actions while a resolve mutation is in flight for this card. */
  busy?: boolean
  onPick: (item: NeedsAttentionItem, candidate: NeedsAttentionCandidate) => void
  onLogImpromptu: (item: NeedsAttentionItem) => void
}

/**
 * A conflict card: the session lede plus a Candidate / Time / Overlap
 * table with an inline "This one" per row and a "None of these — log as
 * impromptu" footer. Table treatment only (cards/radio dropped, per
 * spec). Left rule amber.
 */
export function ConflictCard({ item, busy, onPick, onLogImpromptu }: ConflictCardProps) {
  const candidates = item.candidates ?? []
  return (
    <div className="rounded-lg border border-gray-200 border-l-2 border-l-amber-600 bg-white p-5">
      <SessionLede
        title={item.title}
        meetingAt={item.meeting_at}
        summaryExcerpt={item.summary_excerpt}
      />

      <div className="mt-4">
        <div className="mb-2.5 text-xs font-semibold uppercase tracking-wide text-amber-700">
          Which meeting was this?
        </div>
        <CandidateTable
          candidates={candidates}
          meetingAt={item.meeting_at}
          busy={busy}
          onPick={c => onPick(item, c)}
        />
      </div>

      <div className="mt-3.5 flex justify-end border-t border-gray-100 pt-3.5">
        <Button
          size="sm"
          variant="ghost"
          disabled={busy}
          onClick={() => onLogImpromptu(item)}
          className="text-gray-500"
        >
          None of these — log as impromptu
        </Button>
      </div>
    </div>
  )
}

function CandidateTable({
  candidates,
  meetingAt,
  busy,
  onPick,
}: {
  candidates: NeedsAttentionCandidate[]
  meetingAt: string
  busy?: boolean
  onPick: (c: NeedsAttentionCandidate) => void
}) {
  return (
    <div className="overflow-hidden rounded-lg border border-gray-200">
      <table className="w-full text-[0.8125rem]">
        <thead className="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500">
          <tr>
            <th className="px-3 py-2 font-medium">Candidate</th>
            <th className="px-3 py-2 font-medium">Time</th>
            <th className="px-3 py-2 font-medium">Overlap</th>
            <th className="px-3 py-2" />
          </tr>
        </thead>
        <tbody className="divide-y divide-gray-100">
          {candidates.map(c => (
            <CandidateRow
              key={`${c.kind}-${c.id}`}
              candidate={c}
              meetingAt={meetingAt}
              busy={busy}
              onPick={() => onPick(c)}
            />
          ))}
        </tbody>
      </table>
    </div>
  )
}

function CandidateRow({
  candidate,
  meetingAt,
  busy,
  onPick,
}: {
  candidate: NeedsAttentionCandidate
  meetingAt: string
  busy?: boolean
  onPick: () => void
}) {
  const isPhone = candidate.kind === 'phone_call'
  const Icon = isPhone ? Phone : Calendar
  const preview = candidate.preview
  const attendees = preview?.attendees ?? []
  const heading = isPhone
    ? preview?.peer_handle || 'Phone call'
    : preview?.title || 'Calendar event'

  return (
    <tr>
      <td className="px-3 py-2.5 align-top">
        {candidate.target_missing ? (
          <span className="text-gray-400">This candidate no longer exists</span>
        ) : (
          <div className="flex items-start gap-2">
            <Icon className="mt-0.5 h-3.5 w-3.5 flex-shrink-0 text-gray-400" />
            <div className="min-w-0">
              <div className="font-semibold text-gray-900">{heading}</div>
              {attendees.length > 0 && (
                <div className="mt-0.5 flex flex-wrap gap-x-1.5 text-xs text-gray-500">
                  {attendees.map((a, i) => (
                    <span key={i} className={a.matched ? 'font-semibold text-gray-800' : undefined}>
                      {a.name}
                      {i < attendees.length - 1 ? ',' : ''}
                    </span>
                  ))}
                </div>
              )}
            </div>
          </div>
        )}
      </td>
      <td className="whitespace-nowrap px-3 py-2.5 align-top text-gray-600">
        <div>{formatTime(candidate.occurred_at)}</div>
        <div className="text-xs tabular-nums text-blue-600">
          {formatDrift(candidate.occurred_at, meetingAt)} from session
        </div>
      </td>
      <td className="px-3 py-2.5 align-top">
        <OverlapMeter attendees={attendees} overlapCount={candidate.overlap_count} />
      </td>
      <td className="px-3 py-2.5 text-right align-top">
        <Button
          size="sm"
          variant="outline"
          disabled={busy || candidate.target_missing}
          onClick={onPick}
        >
          <Link2 className="mr-1 h-3.5 w-3.5" />
          This one
        </Button>
      </td>
    </tr>
  )
}

function formatTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
}

/** Client-side drift: candidate time minus the session meeting time,
 * rendered as a signed minute offset. */
function formatDrift(candidateIso: string, meetingIso: string): string {
  const cand = new Date(candidateIso).getTime()
  const meet = new Date(meetingIso).getTime()
  if (Number.isNaN(cand) || Number.isNaN(meet)) return ''
  const mins = Math.round((cand - meet) / 60000)
  const sign = mins >= 0 ? '+' : '−'
  return `${sign}${Math.abs(mins)} min`
}
