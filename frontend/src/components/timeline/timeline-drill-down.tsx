import type { ReactNode } from 'react'
import { useTimelineContent } from '@/hooks/use-interactions'
import type {
  ContentMessageResponse,
  MeetingNoteContentResponse,
  TimelineContentMemberResponse,
} from '@/types/generated/contact'
import { CHANNEL_LABELS, SOURCE_LABELS, formatDateTime } from './format'
import type { ParticipationContext, TimelineIdentity } from './types'
import { visibleMessages } from './visible-messages'

interface TimelineDrillDownProps {
  identity: TimelineIdentity
  context: ParticipationContext
  /** Keyed by a content item's `content_ref`; a key matching no displayed item renders nothing. */
  annotations?: Record<string, ReactNode>
  /** Narrows the displayed messages to one container (IXN-007). Absent, every message shows. */
  venueFilter?: string
}

function Channel({ channel }: { channel: string | null }) {
  if (!channel) return null
  return (
    <span data-channel={channel} className="text-xs text-gray-500">
      {CHANNEL_LABELS[channel] ?? channel}
    </span>
  )
}

function Annotation({ node }: { node: ReactNode | undefined }) {
  if (node == null) return null
  return <div data-annotation>{node}</div>
}

/**
 * One message as the drill-down shows it: its sender and its time on one line,
 * the time lighter, and its text beneath; the user's own sender name is blue.
 */
export function TimelineMessage({
  message,
  annotations,
}: {
  message: ContentMessageResponse
  annotations?: Record<string, ReactNode>
}) {
  return (
    <div data-message-id={message.id} data-content-ref={message.content_ref} className="space-y-1">
      <div className="flex flex-wrap items-baseline gap-x-2">
        <span data-message-sender className={message.is_outgoing ? 'text-blue-700' : undefined}>
          {message.sender}
        </span>
        <span data-message-timestamp className="text-xs text-gray-500">
          {formatDateTime(message.sent_at)}
        </span>
      </div>
      <Channel channel={message.channel} />
      <div data-message-body className="whitespace-pre-wrap">
        {message.body}
      </div>
      <Annotation node={annotations?.[message.content_ref]} />
    </div>
  )
}

/** One meeting note as the drill-down shows it. */
export function TimelineNote({
  note,
  annotations,
}: {
  note: MeetingNoteContentResponse
  annotations?: Record<string, ReactNode>
}) {
  return (
    <div data-meeting-note className="space-y-1">
      {note.title != null && <div data-note-title>{note.title}</div>}
      <Channel channel={note.channel} />
      {note.summary != null && <div data-note-summary>{note.summary}</div>}
      {note.memo != null && (
        <div data-note-memo className="whitespace-pre-wrap">
          {note.memo}
        </div>
      )}
      <Annotation node={annotations?.[note.content_ref]} />
    </div>
  )
}

function MemberSection({
  member,
  annotations,
  venueFilter,
}: {
  member: TimelineContentMemberResponse
  annotations?: Record<string, ReactNode>
  venueFilter?: string
}) {
  const messages = visibleMessages(member.messages, venueFilter)
  const empty = messages.length === 0 && member.meeting_notes.length === 0
  return (
    <section data-member-section={member.source} className="space-y-3">
      <h4 className="text-xs font-medium uppercase text-gray-500">
        {SOURCE_LABELS[member.source] ?? member.source}
      </h4>
      {messages.map(message => (
        <TimelineMessage key={message.id} message={message} annotations={annotations} />
      ))}
      {member.meeting_notes.map(note => (
        <TimelineNote key={note.content_ref} note={note} annotations={annotations} />
      ))}
      {empty && <p className="text-xs text-gray-500">No content to show</p>}
    </section>
  )
}

export function TimelineDrillDown({
  identity,
  context,
  annotations,
  venueFilter,
}: TimelineDrillDownProps) {
  const content = useTimelineContent(identity, context.contactId)
  const hasNotes = content.data?.members.some(member => member.meeting_notes.length > 0)
  return (
    <div data-content-region className="mt-4 rounded bg-gray-50 p-4 text-sm text-gray-700">
      {content.isPending ? (
        'Loading content…'
      ) : content.isError ? (
        'Failed to load content.'
      ) : (
        <div className="space-y-4">
          {content.data.members.map((member, index) => (
            <MemberSection
              key={`${member.source}-${index}`}
              member={member}
              annotations={annotations}
              venueFilter={venueFilter}
            />
          ))}
          {hasNotes && <p>Meeting notes are processed on-device.</p>}
        </div>
      )}
    </div>
  )
}
