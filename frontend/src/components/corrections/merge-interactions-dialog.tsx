'use client'

import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { formatDateTime } from '@/components/timeline/format'
import { useMergeInteractions, useMergePreview } from '@/hooks/use-interactions'
import { getErrorMessage } from '@/lib/utils'
import type { ParticipationContext, TimelineItem } from '@/components/timeline'
import { CorrectionDialog } from './correction-dialog'
import { InteractionSummary } from './interaction-summary'

interface MergeInteractionsDialogProps {
  /** The selected correctable interactions, two or more. */
  items: TimelineItem[]
  context: ParticipationContext
  onClose: () => void
  onMerged: () => void
}

/**
 * Confirms a merge of the selected interactions. The result it states for the
 * contact is the merge preview's answer; the dialog computes none of its own.
 */
export function MergeInteractionsDialog({
  items,
  context,
  onClose,
  onMerged,
}: MergeInteractionsDialogProps) {
  const members = [...items].sort(
    (a, b) => new Date(a.occurred_at).getTime() - new Date(b.occurred_at).getTime()
  )
  const interactionIds = members.map(item => item.identity.id)
  const preview = useMergePreview(interactionIds, context.contactId)
  const merge = useMergeInteractions()
  const [error, setError] = useState<string | null>(null)

  const confirm = async () => {
    setError(null)
    try {
      await merge.mutateAsync({ interactionIds, contactId: context.contactId })
      onMerged()
    } catch (err) {
      setError(getErrorMessage(err, 'Failed to merge the interactions'))
    }
  }

  const result = preview.data?.participation

  return (
    <CorrectionDialog
      title="Merge interactions"
      onClose={onClose}
      footer={
        <>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="button" disabled={!result} loading={merge.isPending} onClick={confirm}>
            Merge
          </Button>
        </>
      }
    >
      <ul aria-label="Interactions to merge" className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {members.map(item => (
          <li key={item.identity.id} className="rounded-md border border-gray-200 p-3">
            <InteractionSummary item={item} />
          </li>
        ))}
      </ul>
      <p className="text-sm text-gray-700">
        Merging records that these are one interaction for every attendee, not only this contact.
        Each attendee keeps only their own part: someone who was in just one of them keeps that
        one&apos;s direction and time. Cadence and follow-up tasks are recalculated for everyone
        affected. A merge can&apos;t be undone.
      </p>
      {preview.isLoading && <p className="text-sm text-gray-500">Working out the result…</p>}
      {preview.error && (
        <p className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-600">
          {getErrorMessage(preview.error, 'These interactions cannot be merged')}
        </p>
      )}
      {result && (
        <p
          data-merge-result
          data-direction={result.direction}
          className="rounded-md bg-blue-50 px-3 py-2 text-sm text-blue-900"
        >
          For this contact, they become one <strong>{result.direction}</strong> interaction at{' '}
          <time dateTime={result.occurred_at}>{formatDateTime(result.occurred_at)}</time>.
        </p>
      )}
      {error && <p className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-600">{error}</p>}
    </CorrectionDialog>
  )
}
