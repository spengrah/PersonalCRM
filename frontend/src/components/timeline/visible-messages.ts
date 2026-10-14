import type { ContentMessageResponse } from '@/types/generated/contact'

/** The messages a drill-down displays: all of them, or only those of one container (IXN-007). */
export function visibleMessages(
  messages: ContentMessageResponse[],
  venueFilter?: string
): ContentMessageResponse[] {
  return messages.filter(message => !venueFilter || message.container === venueFilter)
}
