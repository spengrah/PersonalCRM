import { useState, type ReactNode } from 'react'
import type {
  CallSummaryResponse,
  InteractionDirection,
  TimelineMemberResponse,
} from '@/types/generated/contact'
import { SOURCE_LABELS, formatDateTime, formatTime } from './format'
import { TimelineDrillDown } from './timeline-drill-down'
import type {
  ParticipationContext,
  TimelineIdentity,
  TimelineItem,
  TimelineRowAction,
  TimelineRowSelection,
} from './types'

interface TimelineRowProps {
  item: TimelineItem
  context: ParticipationContext
  actions?: TimelineRowAction[]
  selection?: TimelineRowSelection
  /** The drill-down the row expands. Absent, the row expands TimelineDrillDown. */
  renderDrillDown?: (identity: TimelineIdentity, context: ParticipationContext) => ReactNode
}

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
}

function callServiceLabel(service: string): string {
  if (service === 'facetime_audio') return 'FaceTime audio'
  if (service === 'facetime_video') return 'FaceTime video'
  return 'Voice call'
}

function DirectionGlyph({ direction }: { direction: InteractionDirection }) {
  const glyph = direction === 'inbound' ? '←' : direction === 'outbound' ? '→' : '↔'
  return (
    <span aria-label={direction} title={direction} className="text-gray-500">
      {glyph}
    </span>
  )
}

function ContentIndicator({ member }: { member: TimelineMemberResponse }) {
  if (member.content_kind === 'call') return null
  if (member.content_kind === 'messages') {
    return (
      <span className="text-sm text-gray-500">
        {member.message_count} message{member.message_count === 1 ? '' : 's'}
      </span>
    )
  }
  return (
    <span className="text-sm text-gray-500">
      {member.content_kind === 'meeting_note' ? 'Meeting note' : 'No content'}
    </span>
  )
}

function Caret({ expanded }: { expanded: boolean }) {
  return (
    <svg
      aria-hidden="true"
      width="16"
      height="16"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className={`transition-transform ${expanded ? 'rotate-180' : ''}`}
    >
      <polyline points="6 9 12 15 18 9" />
    </svg>
  )
}

function EventDetails({ member }: { member: TimelineMemberResponse }) {
  if (!member.event) return null
  const { event } = member
  return (
    <div className="mt-2 space-y-1 text-sm text-gray-600">
      <div>
        {formatTime(event.start_time)} - {formatTime(event.end_time)}
      </div>
      {event.location && <div>{event.location}</div>}
      {event.attendee_count > 1 && <div>{event.attendee_count} attendees</div>}
      {event.title && event.html_link ? (
        <a
          href={event.html_link}
          target="_blank"
          rel="noopener noreferrer"
          className="text-blue-600 hover:text-blue-800 hover:underline"
        >
          {event.title}
        </a>
      ) : (
        event.title && <div>{event.title}</div>
      )}
    </div>
  )
}

// "Missed" names an inbound call the user did not answer. A call the user
// placed is connected when it lasted, and otherwise an unanswered attempt.
function callState(call: CallSummaryResponse): string {
  if (call.direction === 'outbound') return call.duration_seconds > 0 ? 'Connected' : 'No answer'
  return call.answered === true ? 'Answered' : 'Missed'
}

function CallDetails({ member }: { member: TimelineMemberResponse }) {
  if (!member.call) return null
  const state = callState(member.call)
  return (
    <div className="mt-2 text-sm text-gray-600">
      <span>{callServiceLabel(member.call.service)}</span>
      <span> · </span>
      <span>{state}</span>
      {member.call.has_voicemail && (
        <>
          <span> · </span>
          <span>Voicemail</span>
        </>
      )}
      <span> · </span>
      <span>{formatDuration(member.call.duration_seconds)}</span>
    </div>
  )
}

function OverflowMenu({
  identity,
  context,
  actions,
}: {
  identity: TimelineIdentity
  context: ParticipationContext
  actions: TimelineRowAction[]
}) {
  const [open, setOpen] = useState(false)
  // The affordance IXN-002 reserves: present and disabled until a host passes actions.
  if (actions.length === 0) {
    return (
      <button
        type="button"
        aria-label="More actions"
        disabled
        className="flex h-8 w-8 items-center justify-center rounded-md text-gray-400 disabled:cursor-not-allowed"
      >
        ⋯
      </button>
    )
  }
  return (
    <div
      className="relative"
      onKeyDown={event => {
        if (event.key === 'Escape') setOpen(false)
      }}
    >
      <button
        type="button"
        aria-label="More actions"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen(value => !value)}
        className="flex h-8 w-8 items-center justify-center rounded-md text-gray-400 hover:bg-gray-100 hover:text-gray-600"
      >
        ⋯
      </button>
      {open && (
        <div
          role="menu"
          className="absolute right-0 z-10 mt-1 w-44 rounded-md bg-white py-1 shadow-lg ring-1 ring-black ring-opacity-5"
        >
          {actions.map(action => (
            <button
              key={action.key}
              type="button"
              role="menuitem"
              onClick={() => {
                setOpen(false)
                action.onSelect(identity, context)
              }}
              className="block w-full px-4 py-2 text-left text-sm text-gray-700 hover:bg-gray-100"
            >
              {action.label}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

export function TimelineRow({
  item,
  context,
  actions,
  selection,
  renderDrillDown,
}: TimelineRowProps) {
  const [expanded, setExpanded] = useState(false)
  const { identity, members } = item
  // The one place that decides whether an item offers corrections.
  const correctable = identity.kind === 'interaction'
  const missedCall = identity.kind === 'missed_call'
  const expandable = members.some(
    member => member.content_kind === 'messages' || member.content_kind === 'meeting_note'
  )

  return (
    <article
      role="listitem"
      {...(missedCall
        ? { 'data-missed-call-id': identity.id }
        : { 'data-interaction-id': identity.id })}
      data-source={members[0]?.source}
      className={missedCall ? 'bg-gray-50 px-4 py-4 sm:px-6' : 'px-4 py-4 sm:px-6'}
    >
      <div className="flex items-start gap-3">
        <DirectionGlyph direction={item.direction} />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
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
            <time dateTime={item.occurred_at} className="text-sm text-gray-500">
              {formatDateTime(item.occurred_at)}
            </time>
            {item.cadence_inert && (
              <span className="text-sm text-gray-500">Not counted toward cadence</span>
            )}
            {!missedCall &&
              members.map((member, index) => (
                <span key={`${member.source}-${index}`} className="contents">
                  <ContentIndicator member={member} />
                  {member.venue_tags.map(tag => (
                    <span
                      key={tag.key}
                      data-venue-key={tag.key}
                      className="rounded-full bg-blue-50 px-2 py-0.5 text-xs text-blue-700"
                    >
                      {tag.label}
                    </span>
                  ))}
                  {member.is_group && (
                    <span className="rounded-full bg-purple-50 px-2 py-0.5 text-xs text-purple-700">
                      Group
                    </span>
                  )}
                </span>
              ))}
          </div>
          {members.map((member, index) => (
            <div key={`${member.source}-${index}`}>
              {!missedCall && (
                <div className="mt-1 text-sm font-medium text-gray-900">{member.label}</div>
              )}
              <EventDetails member={member} />
              <CallDetails member={member} />
            </div>
          ))}
        </div>
        <div data-row-actions className="flex flex-shrink-0 items-center gap-1">
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
          {correctable && (
            <OverflowMenu identity={identity} context={context} actions={actions ?? []} />
          )}
        </div>
      </div>
      {expanded &&
        (renderDrillDown ? (
          renderDrillDown(identity, context)
        ) : (
          <TimelineDrillDown identity={identity} context={context} />
        ))}
    </article>
  )
}
