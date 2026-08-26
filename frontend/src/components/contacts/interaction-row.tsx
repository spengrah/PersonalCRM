import { useState } from 'react'
import type { InteractionListItemResponse } from '@/types/generated/contact'
import { useInteractionContent } from '@/hooks/use-interactions'
import { InteractionContent } from './interaction-content'

const SOURCE_LABELS: Record<string, string> = {
  manual: 'Manual',
  gcal: 'Calendar',
  todoist: 'Todoist',
  telegram: 'Telegram',
  messages: 'iMessage',
  anarlog_sessions: 'Meeting',
  phone_calls: 'Call',
  email: 'Email',
  gchat: 'Google Chat',
  whatsapp: 'WhatsApp',
}

export function formatDateTime(iso: string): string {
  return new Intl.DateTimeFormat('en-US', {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  }).format(new Date(iso))
}

function formatTime(iso: string): string {
  return new Intl.DateTimeFormat('en-US', { hour: 'numeric', minute: '2-digit' }).format(
    new Date(iso)
  )
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

function DirectionGlyph({ direction }: { direction: InteractionListItemResponse['direction'] }) {
  const glyph = direction === 'inbound' ? '←' : direction === 'outbound' ? '→' : '↔'
  return (
    <span aria-label={direction} title={direction} className="text-gray-500">
      {glyph}
    </span>
  )
}

function ContentIndicator({ item }: { item: InteractionListItemResponse }) {
  if (item.content_kind === 'call') return null
  if (item.content_kind === 'messages') {
    return (
      <span className="text-sm text-gray-500">
        {item.message_count} message{item.message_count === 1 ? '' : 's'}
      </span>
    )
  }
  return (
    <span className="text-sm text-gray-500">
      {item.content_kind === 'meeting_note' ? 'Meeting note' : 'No content'}
    </span>
  )
}

function EventDetails({ item }: { item: InteractionListItemResponse }) {
  if (!item.event) return null
  const { event } = item
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

function CallDetails({ item }: { item: InteractionListItemResponse }) {
  if (!item.call) return null
  const state = item.call.answered === true ? 'Answered' : 'Missed'
  return (
    <div className="mt-2 text-sm text-gray-600">
      <span>{callServiceLabel(item.call.service)}</span>
      <span> · </span>
      <span>{state}</span>
      {item.call.has_voicemail && (
        <>
          <span> · </span>
          <span>Voicemail</span>
        </>
      )}
      <span> · </span>
      <span>{formatDuration(item.call.duration_seconds)}</span>
    </div>
  )
}

export function InteractionRow({
  item,
  venueFilter,
}: {
  item: InteractionListItemResponse
  venueFilter?: string
}) {
  const [expanded, setExpanded] = useState(false)
  const expandable = item.content_kind === 'messages' || item.content_kind === 'meeting_note'
  const content = useInteractionContent(item.id, { enabled: expanded && expandable })

  return (
    <article
      role="listitem"
      data-interaction-id={item.id}
      data-source={item.source}
      className="px-4 py-4 sm:px-6"
    >
      <div className="flex items-start gap-3">
        <DirectionGlyph direction={item.direction} />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span
              data-badge
              className="rounded bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-700"
            >
              {SOURCE_LABELS[item.source] ?? item.source}
            </span>
            <time dateTime={item.occurred_at} className="text-sm text-gray-500">
              {formatDateTime(item.occurred_at)}
            </time>
            <ContentIndicator item={item} />
            {expandable && (
              <button
                type="button"
                aria-expanded={expanded}
                onClick={() => setExpanded(value => !value)}
                className="rounded border border-gray-300 px-2 py-0.5 text-sm text-gray-700"
              >
                {expanded ? 'Collapse content' : 'Expand content'}
              </button>
            )}
            {item.venue_tags.map(tag => (
              <span
                key={tag.key}
                data-venue-key={tag.key}
                className="rounded-full bg-blue-50 px-2 py-0.5 text-xs text-blue-700"
              >
                {tag.label}
              </span>
            ))}
            {item.is_group && (
              <span className="rounded-full bg-purple-50 px-2 py-0.5 text-xs text-purple-700">
                Group
              </span>
            )}
          </div>
          <div className="mt-1 text-sm font-medium text-gray-900">{item.label}</div>
          <EventDetails item={item} />
          <CallDetails item={item} />
        </div>
        <button
          type="button"
          aria-label="More actions"
          disabled
          className="rounded p-1 text-gray-400 disabled:cursor-not-allowed"
        >
          ⋯
        </button>
      </div>
      {expanded && (
        <InteractionContent
          isPending={content.isPending}
          isError={content.isError}
          content={content.data}
          venueFilter={venueFilter}
        />
      )}
    </article>
  )
}
