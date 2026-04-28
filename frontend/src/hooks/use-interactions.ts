import { useMutation } from '@tanstack/react-query'
import { interactionsApi, type CreateInteractionRequest } from '@/lib/interactions-api'
import { invalidateFor } from '@/lib/query-invalidation'

// useCreateInteraction posts a manual interaction for a contact and
// invalidates the caches that depend on cadence-column state. Used by
// the contact-detail Log Interaction modal and by the dashboard /
// contact-list "Mark as Contacted" quick actions.
export function useCreateInteraction() {
  return useMutation({
    mutationFn: ({ contactId, data }: { contactId: string; data: CreateInteractionRequest }) =>
      interactionsApi.create(contactId, data),
    onSuccess: (_resp, vars) => {
      // Fire both events:
      //   interaction:created — the new domain event for cadence/list/detail caches.
      //   contact:touched     — preserved so legacy listeners (overdue dashboard,
      //                         etc.) keep working without a same-PR audit. A
      //                         follow-up PR can collapse the events once the
      //                         listener inventory is settled.
      invalidateFor('interaction:created', vars.contactId)
      invalidateFor('contact:touched', vars.contactId)
    },
  })
}
