import { TimelineDrillDown } from '@/components/timeline'
import type { ParticipationContext, TimelineIdentity, TimelineItem } from '@/components/timeline'
import { visibleMessages } from '@/components/timeline/visible-messages'
import { useTimelineAssertions, useTimelineContent } from '@/hooks/use-interactions'
import { assertionAnnotations } from './content-assertions'

interface InteractionDrillDownProps {
  item: TimelineItem
  identity: TimelineIdentity
  context: ParticipationContext
  venueFilter?: string
}

// The contact page's drill-down: the timeline drill-down plus, above it, the
// disclosure that the thread window shown and the messages recorded against the
// interaction are drawn from different sets. The recorded count is the row's;
// the shown count is what the drill-down displays (the same cached read). The
// assertions citing a displayed content item show beside it (IXN-023).
export function InteractionDrillDown({
  item,
  identity,
  context,
  venueFilter,
}: InteractionDrillDownProps) {
  const content = useTimelineContent(identity, context.contactId)
  const assertions = useTimelineAssertions(identity, context.contactId)
  const messageMembers = content.data?.members.filter(member => member.kind === 'messages') ?? []
  const shown = messageMembers.reduce(
    (total, member) => total + visibleMessages(member.messages, venueFilter).length,
    0
  )
  const recorded = item.members.reduce((total, member) => total + member.message_count, 0)
  return (
    <>
      {messageMembers.length > 0 && shown !== recorded && (
        <p data-evidence-scope className="mt-4 text-xs text-gray-500">
          Showing {shown} message{shown === 1 ? '' : 's'} from this conversation · {recorded}{' '}
          recorded for this interaction
        </p>
      )}
      <TimelineDrillDown
        identity={identity}
        context={context}
        venueFilter={venueFilter}
        annotations={assertions.data && assertionAnnotations(assertions.data.items)}
      />
    </>
  )
}
