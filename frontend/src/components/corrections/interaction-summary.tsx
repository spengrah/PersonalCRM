import { SOURCE_LABELS, formatDateTime } from '@/components/timeline/format'
import type { TimelineItem } from '@/components/timeline'

/** Names one timeline item in a correction: its sources, labels, direction and time. */
export function InteractionSummary({ item }: { item: TimelineItem }) {
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-2">
        {item.members.map((member, index) => (
          <span
            key={`${member.source}-${index}`}
            className="rounded bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-700"
          >
            {SOURCE_LABELS[member.source] ?? member.source}
          </span>
        ))}
      </div>
      {item.members.map((member, index) => (
        <div key={`${member.source}-${index}`} className="text-sm font-medium text-gray-900">
          {member.label}
        </div>
      ))}
      <div className="text-sm text-gray-600">
        <span className="capitalize">{item.direction}</span>
        <span> · </span>
        <time dateTime={item.occurred_at}>{formatDateTime(item.occurred_at)}</time>
      </div>
    </div>
  )
}
