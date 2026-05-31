import { useMutation, useQuery } from '@tanstack/react-query'
import { importsApi } from '@/lib/imports-api'
import { importKeys, invalidateFor } from '@/lib/query-invalidation'
import type { ResolveLinkRequest, ResolveDiscoveryTokenRequest } from '@/types/import'

// Interactions tab: the conflict + orphan queue (all hosts).
export function useInteractionsQueue() {
  return useQuery({
    queryKey: importKeys.needsAttention(),
    queryFn: () => importsApi.getNeedsAttention(),
    staleTime: 1000 * 60 * 2, // 2 minutes
  })
}

// Resolve a conflict/orphan (This one / None of these / Log as impromptu).
// Always invalidates the queue + badge + contact lists/overdue, even when
// zero walk-in interactions were created; additionally refreshes each
// affected contact's detail + tasks when interactions_created is non-empty.
export function useResolveLink() {
  return useMutation({
    mutationFn: ({ id, request }: { id: string; request: ResolveLinkRequest }) =>
      importsApi.resolveLink(id, request),
    onSuccess: response => {
      // Static keys fire once (idempotent on the per-contact pass below).
      invalidateFor('meeting-note:resolved')
      // Per affected contact: refresh detail + tasks for any walk-in created.
      const seen = new Set<string>()
      for (const interaction of response.interactions_created ?? []) {
        if (seen.has(interaction.contact_id)) continue
        seen.add(interaction.contact_id)
        invalidateFor('meeting-note:resolved', interaction.contact_id)
      }
    },
  })
}

// People tab: grouped anarlog_title discovery tokens.
export function useAnarlogTitleDiscovery() {
  return useQuery({
    queryKey: importKeys.anarlogTitle(),
    queryFn: () => importsApi.getAnarlogTitleGroups(),
    staleTime: 1000 * 60 * 2, // 2 minutes
  })
}

// Resolve a whole anarlog_title token group (import / link / ignore).
// Invalidates the discovery list + contact lists/overdue; and the affected
// contact's detail key when the response carries a contact_id (import:
// created id; link: crm_contact_id).
export function useResolveDiscoveryToken() {
  return useMutation({
    mutationFn: (request: ResolveDiscoveryTokenRequest) =>
      importsApi.resolveDiscoveryToken(request),
    onSuccess: response => {
      invalidateFor('discovery:resolved', response.contact_id)
    },
  })
}
