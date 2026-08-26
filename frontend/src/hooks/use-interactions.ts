import { useInfiniteQuery, useMutation, useQuery } from '@tanstack/react-query'
import {
  interactionsApi,
  type CreateInteractionRequest,
  type InteractionListFilters,
} from '@/lib/interactions-api'
import { invalidateFor } from '@/lib/query-invalidation'
import { interactionKeys } from '@/lib/query-keys'
import { staleTime } from '@/lib/query-client'

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

export function useContactInteractions(contactId: string, filters: InteractionListFilters = {}) {
  return useInfiniteQuery({
    queryKey: interactionKeys.list(contactId, filters),
    queryFn: ({ pageParam }) => interactionsApi.list(contactId, { ...filters, page: pageParam }),
    initialPageParam: 1,
    getNextPageParam: last => {
      const p = last.meta?.pagination
      return p && p.page < p.pages ? p.page + 1 : undefined
    },
    enabled: !!contactId,
    staleTime: staleTime(1000 * 60 * 5),
  })
}

export function useInteractionContent(interactionId: string, opts: { enabled: boolean }) {
  return useQuery({
    queryKey: interactionKeys.content(interactionId),
    queryFn: () => interactionsApi.getContent(interactionId),
    enabled: opts.enabled && !!interactionId,
    staleTime: staleTime(1000 * 60 * 5),
  })
}
