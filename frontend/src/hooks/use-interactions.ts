import { useMutation } from '@tanstack/react-query'
import { interactionsApi, type CreateInteractionRequest } from '@/lib/interactions-api'
import { invalidateFor } from '@/lib/query-invalidation'

// useCreateInteraction posts a manual interaction for a contact and
// invalidates the caches that depend on cadence-column state. Used by
// the contact-detail Log Interaction modal and by the dashboard /
// contact-list "Mark as Contacted" quick actions. The
// `interaction:created` invalidation rule is a strict superset of the
// legacy `contact:touched` rule (adds per-contact detail + task list
// invalidation), so a single fire is sufficient.
export function useCreateInteraction() {
  return useMutation({
    mutationFn: ({ contactId, data }: { contactId: string; data: CreateInteractionRequest }) =>
      interactionsApi.create(contactId, data),
    onSuccess: (_resp, vars) => {
      invalidateFor('interaction:created', vars.contactId)
    },
  })
}
