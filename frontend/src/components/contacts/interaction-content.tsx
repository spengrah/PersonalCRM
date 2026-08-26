import type { InteractionContentResponse } from '@/types/generated/contact'
import { formatDateTime } from './interaction-row'

interface InteractionContentProps {
  isPending: boolean
  isError: boolean
  content?: InteractionContentResponse
  venueFilter?: string
  /** The row's own message count — how many messages are recorded against this
   * interaction. The expanded evidence is drawn from the surrounding thread
   * window and a venue filter narrows it further, so the two legitimately
   * differ; when they do, say so rather than letting the row's count read as a
   * promise about what is displayed. */
  recordedCount?: number
}

export function InteractionContent({
  isPending,
  isError,
  content,
  venueFilter,
  recordedCount,
}: InteractionContentProps) {
  const visible =
    content?.kind === 'messages'
      ? content.messages.filter(message => !venueFilter || message.venue_key === venueFilter)
      : []
  const countsDiffer = recordedCount != null && visible.length !== recordedCount
  return (
    <div data-content-region className="mt-4 rounded bg-gray-50 p-4 text-sm text-gray-700">
      {isPending ? (
        'Loading content…'
      ) : isError ? (
        'Failed to load content.'
      ) : content?.kind === 'messages' ? (
        <div className="space-y-3">
          {countsDiffer && (
            <p data-evidence-scope className="text-xs text-gray-500">
              Showing {visible.length} message{visible.length === 1 ? '' : 's'} from this
              conversation · {recordedCount} recorded for this interaction
            </p>
          )}
          {visible.map(message => (
            <div key={message.id} data-message-id={message.id} className="space-y-1">
              <div data-message-sender>{message.sender}</div>
              <div data-message-timestamp>{formatDateTime(message.sent_at)}</div>
              <div data-message-body className="whitespace-pre-wrap">
                {message.body}
              </div>
            </div>
          ))}
        </div>
      ) : content?.kind === 'meeting_note' ? (
        <div className="space-y-3">
          {content.meeting_notes.map((note, index) => (
            <div key={`${note.title ?? 'note'}-${index}`} data-meeting-note className="space-y-1">
              {note.title != null && <div data-note-title>{note.title}</div>}
              {note.summary != null && <div data-note-summary>{note.summary}</div>}
              {note.memo != null && (
                <div data-note-memo className="whitespace-pre-wrap">
                  {note.memo}
                </div>
              )}
            </div>
          ))}
          <p>Meeting notes are processed on-device.</p>
        </div>
      ) : null}
    </div>
  )
}
