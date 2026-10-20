'use client'

import { useState } from 'react'
import Link from 'next/link'
import { clsx } from 'clsx'
import {
  CallDetails,
  Caret,
  DirectionGlyph,
  OverflowMenu,
  SOURCE_LABELS,
  TimelineDrillDown,
  formatDateTime,
  type ParticipationContext,
  type TimelineRowAction,
  type TimelineRowSelection,
} from '@/components/timeline'
import type { AllInteractionItemResponse } from '@/types/generated/contact'

interface InteractionRowProps {
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

/**
 * One row of the Interactions tab: the contact history's row parts laid out
 * across the full width (direction and source, participants, preview, date,
 * actions), read in the row's context contact's participation, and its
 * drill-down beneath. While the row is open its header stays in view, so the
 * user can close it from anywhere in the drill-down. The row carries no
 * judgment.
 */
export function InteractionRow({ row, actions, selection }: InteractionRowProps) {
  const [expanded, setExpanded] = useState(false)
  const { identity, members } = row.item
  const context: ParticipationContext = { contactId: row.context_contact_id }
  const missedCall = identity.kind === 'missed_call'
  const correctable = identity.kind === 'interaction'
  const expandable = members.some(
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
      <div
        data-row-header
        className={clsx(
          'flex flex-col gap-2 lg:grid lg:grid-cols-[170px_200px_minmax(0,1fr)_168px_72px] lg:items-start lg:gap-4',
          expanded && 'sticky top-16 z-10 py-2',
          expanded && (missedCall ? 'bg-gray-50' : 'bg-white')
        )}
      >
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
              onClick={() => setExpanded(!expanded)}
              className="flex h-8 w-8 items-center justify-center rounded-md text-gray-400 hover:bg-gray-100 hover:text-gray-600"
            >
              <Caret expanded={expanded} />
            </button>
          )}
          {correctable && <OverflowMenu identity={identity} context={context} actions={actions} />}
        </div>
      </div>
      {expanded && <TimelineDrillDown identity={identity} context={context} />}
    </article>
  )
}
