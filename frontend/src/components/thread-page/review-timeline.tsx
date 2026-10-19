import type {
  NightResponse,
  ThreadMessageResponse,
  ThreadReviewResponse,
} from '@/types/generated/contact'
import { Collapsed } from './collapsed'
import { JudgmentBox } from './judgment-box'
import { MessageFlags } from './flag-chips'
import { Message } from './message'
import { count, formatDays } from './night-label'

interface ReviewTimelineProps {
  threadId: string
  review: ThreadReviewResponse
  messages: ThreadMessageResponse[]
  /** The message a link has just taken the user to. */
  highlighted: string | null
  onShow: (messageKey: string) => void
}

/** "{night} · {n} new messages": the days of the night's messages, and the server's count of its new messages. */
function NightDivider({
  night,
  placed,
}: {
  night: NightResponse
  placed: ThreadMessageResponse[]
}) {
  const days =
    placed.length > 0
      ? formatDays(placed[0].sent_at, placed[placed.length - 1].sent_at)
      : formatDays(night.cut_off, night.cut_off)
  return (
    <div
      role="separator"
      data-night-divider
      className="flex items-center gap-3 py-3 text-xs text-gray-500"
    >
      <span className="h-px flex-1 bg-gray-200" />
      <span>
        <span className="font-semibold text-gray-700">{days}</span>
        {` · ${count(night.new_message_count, 'new message')}`}
      </span>
      <span className="h-px flex-1 bg-gray-200" />
    </div>
  )
}

/**
 * The thread as the run's nights read it, each message once in thread order:
 * the messages before the first night plain; for each night a divider, its new
 * messages with their chips and "+ Flag", and its judgment box; the messages
 * after the last night behind a toggle. The server places every message.
 */
export function ReviewTimeline({
  threadId,
  review,
  messages,
  highlighted,
  onShow,
}: ReviewTimelineProps) {
  const byKey = new Map(messages.map(message => [message.message_key, message]))
  const inNight = new Set(review.nights.flatMap(night => night.messages.map(m => m.message_key)))
  const later = new Set(review.later_messages)
  const before = messages.filter(
    message => !inNight.has(message.message_key) && !later.has(message.message_key)
  )
  const after = messages.filter(message => later.has(message.message_key))

  return (
    <div data-review-timeline className="rounded-lg bg-white p-4 shadow">
      {before.map(message => (
        <Message
          key={message.message_key}
          message={message}
          highlighted={highlighted === message.message_key}
        />
      ))}
      {review.nights.map(night => {
        const placed = night.messages.flatMap(m => byKey.get(m.message_key) ?? [])
        return (
          <section key={night.judgment_id} data-night={night.judgment_id}>
            <NightDivider night={night} placed={placed} />
            {night.messages.map(placedMessage => {
              const message = byKey.get(placedMessage.message_key)
              if (!message) return null
              return (
                <Message
                  key={message.message_key}
                  message={message}
                  highlighted={highlighted === message.message_key}
                >
                  <div data-judged={placedMessage.judged}>
                    <MessageFlags
                      threadId={threadId}
                      runId={review.run.id}
                      messageKey={message.message_key}
                      chips={placedMessage.chips}
                    />
                  </div>
                </Message>
              )
            })}
            <div className="py-3">
              <JudgmentBox
                threadId={threadId}
                runId={review.run.id}
                night={night}
                messages={byKey}
                onShow={onShow}
              />
            </div>
          </section>
        )
      })}
      {after.length > 0 && (
        <Collapsed label={`${count(after.length, 'later message')}, after this run's last night`}>
          {after.map(message => (
            <Message
              key={message.message_key}
              message={message}
              highlighted={highlighted === message.message_key}
            />
          ))}
        </Collapsed>
      )}
    </div>
  )
}
