import { clsx } from 'clsx'
import type { ReactNode } from 'react'
import { formatDateTime } from '@/components/timeline'
import type { ThreadMessageResponse } from '@/types/generated/contact'

/** The sender the thread read names the user by. */
const ME = 'Me'

/**
 * One message as the thread page draws it: "{sender} · {time}" on one line, the
 * sender semibold and blue for the user's own, then the text. Children, the
 * message's chips, follow the text. The message is the target of every link to
 * it, and shows highlighted while a link has just taken the user there.
 */
export function Message({
  message,
  highlighted = false,
  children,
}: {
  message: ThreadMessageResponse
  highlighted?: boolean
  children?: ReactNode
}) {
  return (
    <div
      id={messageDomId(message.message_key)}
      data-message-key={message.message_key}
      data-highlighted={highlighted || undefined}
      className={clsx(
        'scroll-mt-48 space-y-1 rounded-md px-3 py-2 transition-colors',
        highlighted && 'bg-yellow-100'
      )}
    >
      <div className="flex flex-wrap items-baseline gap-x-1 text-xs text-gray-500">
        <span
          data-message-sender
          className={clsx(
            'font-semibold',
            message.sender === ME ? 'text-blue-700' : 'text-gray-700'
          )}
        >
          {message.sender}
        </span>
        <span aria-hidden="true">·</span>
        <time data-message-time dateTime={message.sent_at}>
          {formatDateTime(message.sent_at)}
        </time>
      </div>
      <div data-message-text className="whitespace-pre-wrap text-[15px] text-gray-900">
        {message.text}
      </div>
      {children}
    </div>
  )
}

export function messageDomId(messageKey: string): string {
  return `message-${messageKey}`
}
