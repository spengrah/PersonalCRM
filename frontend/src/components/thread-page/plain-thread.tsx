import type { ThreadMessageResponse } from '@/types/generated/contact'
import { Collapsed } from './collapsed'
import { Message } from './message'
import { count } from './night-label'

/** The messages a thread first shows with Review off; the rest sit behind the earlier-messages toggle. */
export const RECENT_MESSAGES = 20

/**
 * The thread with Review off: its messages in order in one card, the older ones
 * behind a collapsed "{n} earlier messages" toggle at the top.
 */
export function PlainThread({ messages }: { messages: ThreadMessageResponse[] }) {
  const earlier = messages.slice(0, Math.max(0, messages.length - RECENT_MESSAGES))
  const recent = messages.slice(earlier.length)
  return (
    <div data-plain-thread className="space-y-1 rounded-lg bg-white p-4 shadow">
      {earlier.length > 0 && (
        <Collapsed label={count(earlier.length, 'earlier message')}>
          {earlier.map(message => (
            <Message key={message.message_key} message={message} />
          ))}
        </Collapsed>
      )}
      {recent.map(message => (
        <Message key={message.message_key} message={message} />
      ))}
    </div>
  )
}
