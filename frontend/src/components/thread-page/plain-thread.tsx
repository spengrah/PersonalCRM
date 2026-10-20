import type { ThreadMessageResponse } from '@/types/generated/contact'
import { Collapsed } from './collapsed'
import { Message } from './message'

/** The messages a thread first shows with Review off; the rest sit behind "Earlier messages". */
export const RECENT_MESSAGES = 20

/**
 * The thread with Review off: its messages in order in one card, the older ones
 * behind a collapsed "Earlier messages" toggle at the top.
 */
export function PlainThread({ messages }: { messages: ThreadMessageResponse[] }) {
  const earlier = messages.slice(0, Math.max(0, messages.length - RECENT_MESSAGES))
  const recent = messages.slice(earlier.length)
  return (
    <div data-plain-thread className="space-y-1 rounded-lg bg-white p-4 shadow">
      {earlier.length > 0 && (
        <Collapsed label="Earlier messages" note={`${earlier.length}`}>
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
