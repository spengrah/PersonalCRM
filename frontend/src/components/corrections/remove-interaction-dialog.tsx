'use client'

import { useId, useState } from 'react'
import { Button } from '@/components/ui/button'
import { useInteractionParticipants, useRemoveParticipations } from '@/hooks/use-interactions'
import { getErrorMessage } from '@/lib/utils'
import type { InteractionRemovalRequest } from '@/types/generated/contact'
import type { ParticipationContext, TimelineItem } from '@/components/timeline'
import { CorrectionDialog } from './correction-dialog'
import { InteractionSummary } from './interaction-summary'

type RemovalReason = InteractionRemovalRequest['reason']

const REASONS: { value: RemovalReason; label: string; description: string }[] = [
  { value: 'duplicate', label: 'Duplicate', description: 'It repeats another interaction.' },
  {
    value: 'did_not_attend',
    label: "Didn't attend",
    description: "The selected attendees weren't there.",
  },
  { value: 'did_not_happen', label: "Didn't happen", description: 'It was cancelled.' },
]

interface RemoveInteractionDialogProps {
  item: TimelineItem
  context: ParticipationContext
  onClose: () => void
}

/**
 * Removes participations of one correctable interaction. The attendee
 * selection alone decides what is removed; the reason is recorded beside it.
 */
export function RemoveInteractionDialog({ item, context, onClose }: RemoveInteractionDialogProps) {
  const participants = useInteractionParticipants(item.identity.id)
  const remove = useRemoveParticipations()
  const [selected, setSelected] = useState<string[]>([context.contactId])
  const [everyone, setEveryone] = useState(false)
  const [reason, setReason] = useState<RemovalReason | null>(null)
  const [error, setError] = useState<string | null>(null)
  const reasonDescriptionId = useId()

  const attendees = participants.data?.participants ?? []
  const selectedAttendees = attendees.filter(p => selected.includes(p.contact_id))
  const canSubmit = reason !== null && (everyone || selectedAttendees.length > 0)

  const toggle = (contactId: string) =>
    setSelected(current =>
      current.includes(contactId) ? current.filter(id => id !== contactId) : [...current, contactId]
    )

  const submit = async () => {
    if (!reason) return
    setError(null)
    const scope = everyone
      ? { everyone: true }
      : { contact_ids: selectedAttendees.map(p => p.contact_id) }
    try {
      await remove.mutateAsync({ interactionId: item.identity.id, body: { scope, reason } })
      onClose()
    } catch (err) {
      setError(getErrorMessage(err, 'Failed to remove the interaction'))
    }
  }

  return (
    <CorrectionDialog
      title="Remove interaction"
      onClose={onClose}
      footer={
        <>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="button"
            variant="danger"
            disabled={!canSubmit}
            loading={remove.isPending}
            onClick={submit}
          >
            Remove
          </Button>
        </>
      }
    >
      <InteractionSummary item={item} />
      <p className="text-sm text-gray-700">
        This takes the interaction off the timeline of each attendee you select. Their cadence is
        recalculated from the interactions that remain, and follow-up tasks are updated to match.
        The interaction&apos;s content stays stored. A removal can&apos;t be undone.
      </p>

      <fieldset className="space-y-2">
        <legend className="text-sm font-medium text-gray-900">Attendees</legend>
        {participants.isLoading && <p className="text-sm text-gray-500">Loading attendees…</p>}
        {participants.error && (
          <p className="text-sm text-red-700">Failed to load the attendees.</p>
        )}
        {attendees.map(participant => (
          <label key={participant.contact_id} className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={everyone || selected.includes(participant.contact_id)}
              disabled={everyone}
              onChange={() => toggle(participant.contact_id)}
            />
            <span>
              {participant.name}
              {participant.contact_id === context.contactId && (
                <span className="text-gray-500"> (this contact)</span>
              )}
            </span>
          </label>
        ))}
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={everyone} onChange={() => setEveryone(value => !value)} />
          <span>Everyone, including anyone a later sync adds to this interaction</span>
        </label>
      </fieldset>

      <fieldset className="space-y-2" aria-describedby={reasonDescriptionId}>
        <legend className="text-sm font-medium text-gray-900">Reason</legend>
        <p id={reasonDescriptionId} className="text-xs text-gray-500">
          The reason is recorded with the removal. It doesn&apos;t change who is removed.
        </p>
        {REASONS.map(option => (
          <label key={option.value} className="flex items-start gap-2 text-sm">
            <input
              type="radio"
              name="removal-reason"
              value={option.value}
              checked={reason === option.value}
              onChange={() => setReason(option.value)}
              className="mt-0.5"
            />
            <span>
              {option.label}
              <span className="text-gray-500"> — {option.description}</span>
            </span>
          </label>
        ))}
      </fieldset>

      {error && <p className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-600">{error}</p>}
    </CorrectionDialog>
  )
}
