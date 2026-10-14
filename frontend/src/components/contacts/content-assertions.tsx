import type { ReactNode } from 'react'
import type { AssertionStatus } from '@/types/generated/assertion'
import type { TimelineAssertionResponse } from '@/types/generated/contact'

// Exhaustive over the store's status vocabulary, which the generated union
// carries: a new status fails the type check here until it has a label.
const STATUS_LABELS: Record<AssertionStatus, string> = {
  proposed: 'Proposed',
  accepted: 'Accepted',
  rejected: 'Rejected',
  superseded: 'Superseded',
  retracted: 'Retracted',
}

// An edge's object, or a fact's one value.
function assertionValue(assertion: TimelineAssertionResponse): string {
  if (assertion.object != null) return assertion.object
  if (assertion.value_text != null) return assertion.value_text
  if (assertion.value_num != null) return String(assertion.value_num)
  if (assertion.value_date != null) return assertion.value_date
  if (assertion.value_bool != null) return assertion.value_bool ? 'yes' : 'no'
  return ''
}

// The assertions citing one content item, read-only: each states what it
// claims and its recorded review status, and offers nothing to act on.
function ContentAssertions({ assertions }: { assertions: TimelineAssertionResponse[] }) {
  return (
    <ul
      aria-label="Assertions citing this content"
      className="mt-2 space-y-1 border-l-2 border-gray-200 pl-3 text-xs text-gray-600"
    >
      {assertions.map(assertion => (
        <li key={assertion.assertion_id} data-assertion-id={assertion.assertion_id}>
          <span data-assertion-subject>{assertion.subject}</span>
          <span> · </span>
          <span data-assertion-predicate>{assertion.predicate.split('_').join(' ')}</span>
          <span> · </span>
          <span data-assertion-value>{assertionValue(assertion)}</span>
          <span data-assertion-status className="ml-2 rounded bg-gray-100 px-1.5 py-0.5">
            {STATUS_LABELS[assertion.status]}
          </span>
        </li>
      ))}
    </ul>
  )
}

// Groups the assertions by the content item they cite, as drill-down
// annotations keyed by content_ref.
export function assertionAnnotations(
  assertions: TimelineAssertionResponse[]
): Record<string, ReactNode> {
  const byRef = new Map<string, TimelineAssertionResponse[]>()
  for (const assertion of assertions) {
    byRef.set(assertion.content_ref, [...(byRef.get(assertion.content_ref) ?? []), assertion])
  }
  return Object.fromEntries(
    Array.from(byRef, ([ref, cited]) => [ref, <ContentAssertions key={ref} assertions={cited} />])
  )
}
