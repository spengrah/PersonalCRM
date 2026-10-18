'use client'

import { useState } from 'react'
import Link from 'next/link'
import { Check } from 'lucide-react'
import {
  CallDetails,
  Caret,
  DirectionGlyph,
  OverflowMenu,
  SOURCE_LABELS,
  formatDateTime,
  type ParticipationContext,
  type TimelineRowAction,
  type TimelineRowSelection,
} from '@/components/timeline'
import type { AllInteractionItemResponse } from '@/types/generated/contact'
import { BenchDrillDown } from './bench-drill-down'

interface BenchRowProps {
  row: AllInteractionItemResponse
  actions: TimelineRowAction[]
  selection?: TimelineRowSelection
}

function Preview({ row }: { row: AllInteractionItemResponse }) {
  const member = row.item.members[0]
  if (row.preview?.note) return <>Meeting note · {row.preview.text}</>
  if (row.preview) return <>{`${row.preview.sender}: ${row.preview.text}`}</>
  if (member?.call) return <CallDetails member={member} />
  return <>No content</>
}

function Count({ row }: { row: AllInteractionItemResponse }) {
  const messages = row.item.members.reduce((total, member) => total + member.message_count, 0)
  const attendees = row.item.members.find(member => member.event)?.event?.attendee_count ?? 0
  if (messages > 0) return <>{`${messages} message${messages === 1 ? '' : 's'}`}</>
  if (attendees > 1) return <>{`${attendees} attendees`}</>
  return null
}

/** The row's review status as the server compiled it; a row with no proposal has none. */
function ReviewIndicator({ row }: { row: AllInteractionItemResponse }) {
  if (!row.review_status) return null
  if (row.review_status !== 'all_reviewed') {
    const toReview = row.review_status === 'to_review'
    return (
      <span
        data-review-indicator={toReview ? 'to-review' : 'nones-to-review'}
        className="inline-flex whitespace-nowrap rounded-full border border-amber-300 bg-amber-50 px-2 py-0.5 text-xs font-medium text-amber-800"
      >
        {toReview ? `${row.to_review} to review` : 'Nones to review'}
      </span>
    )
  }
  return (
    <span
      data-review-indicator="reviewed"
      className="inline-flex items-center gap-1 text-xs text-gray-500"
    >
      <Check aria-hidden="true" className="h-3.5 w-3.5" />
      Reviewed
    </span>
  )
}

/**
 * One row of the all-interactions view: the contact history's row parts laid
 * out across the full width (direction and source, participants, preview,
 * date, review indicator, actions), read in the row's context contact's
 * participation, and its drill-down beneath.
 */
export function BenchRow({ row, actions, selection }: BenchRowProps) {
  const [expanded, setExpanded] = useState(false)
  const { identity, members } = row.item
  const context: ParticipationContext = { contactId: row.context_contact_id }
  const missedCall = identity.kind === 'missed_call'
  const correctable = identity.kind === 'interaction'
  const expandable =
    row.proposals.length > 0 ||
    members.some(
      member => member.content_kind === 'messages' || member.content_kind === 'meeting_note'
    )
  const group = members.some(member => member.is_group)

  return (
    <article
      role="listitem"
      {...(missedCall
        ? { 'data-missed-call-id': identity.id }
        : { 'data-interaction-id': identity.id })}
      data-source={members[0]?.source}
      className={missedCall ? 'bg-gray-50 px-4 py-4 sm:px-6' : 'px-4 py-4 sm:px-6'}
    >
      <div className="flex flex-col gap-2 lg:grid lg:grid-cols-[170px_200px_minmax(0,1fr)_168px_104px_72px] lg:items-start lg:gap-4">
        <div className="flex flex-wrap items-center gap-2">
          <DirectionGlyph direction={row.item.direction} />
          {members.map((member, index) => (
            <span
              key={`${member.source}-${index}`}
              data-badge
              className={
                missedCall
                  ? 'rounded bg-amber-50 px-2 py-0.5 text-xs font-medium text-amber-800'
                  : 'rounded bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-700'
              }
            >
              {missedCall ? member.label : (SOURCE_LABELS[member.source] ?? member.source)}
            </span>
          ))}
          {group && (
            <span className="rounded-full bg-purple-50 px-2 py-0.5 text-xs text-purple-700">
              Group
            </span>
          )}
        </div>
        <div data-participants className="min-w-0 truncate text-sm">
          {row.participants.map((participant, index) => (
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
        <div className="min-w-0">
          {!missedCall && (
            <div className="truncate text-sm font-medium text-gray-900">{members[0]?.label}</div>
          )}
          <div data-preview className="truncate text-sm text-gray-600">
            <Preview row={row} />
          </div>
        </div>
        <div className="text-sm text-gray-500">
          <time dateTime={row.item.occurred_at}>{formatDateTime(row.item.occurred_at)}</time>
          <div>
            <Count row={row} />
          </div>
        </div>
        <div className="lg:text-right">
          <ReviewIndicator row={row} />
        </div>
        <div data-row-actions className="flex items-center justify-end gap-1">
          {correctable && selection && (
            <input
              type="checkbox"
              aria-label="Select interaction"
              checked={selection.selected}
              onChange={() => selection.onToggle(identity)}
            />
          )}
          {expandable && (
            <button
              type="button"
              aria-expanded={expanded}
              aria-label={expanded ? 'Collapse content' : 'Expand content'}
              onClick={() => setExpanded(value => !value)}
              className="flex h-8 w-8 items-center justify-center rounded-md text-gray-400 hover:bg-gray-100 hover:text-gray-600"
            >
              <Caret expanded={expanded} />
            </button>
          )}
          {correctable && <OverflowMenu identity={identity} context={context} actions={actions} />}
        </div>
      </div>
      {expanded && <BenchDrillDown row={row} context={context} />}
    </article>
  )
}
