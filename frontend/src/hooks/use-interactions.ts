import { useInfiniteQuery, useMutation, useQuery } from '@tanstack/react-query'
import {
  interactionsApi,
  type CreateInteractionRequest,
  type InteractionListFilters,
} from '@/lib/interactions-api'
import { invalidateFor } from '@/lib/query-invalidation'
import { interactionKeys } from '@/lib/query-keys'
import { staleTime } from '@/lib/query-client'
import type { InteractionRemovalRequest, TimelineIdentityResponse } from '@/types/generated/contact'

// useCreateInteraction posts a manual interaction for a contact and
// invalidates the caches that depend on cadence-column state. Used by
// the contact-detail Log Interaction modal and by the contact-list
// "Mark as Contacted" quick action. The
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

export function useTimelineContent(identity: TimelineIdentityResponse, contactId: string) {
  return useQuery({
    queryKey: interactionKeys.timelineContent(contactId, identity.kind, identity.id),
    queryFn: () => interactionsApi.getTimelineContent(identity, contactId),
    enabled: !!identity.id && !!contactId,
    staleTime: staleTime(1000 * 60 * 5),
  })
}

export function useInteractionParticipants(interactionId: string) {
  return useQuery({
    queryKey: interactionKeys.participants(interactionId),
    queryFn: () => interactionsApi.getParticipants(interactionId),
    enabled: !!interactionId,
  })
}

// The merge preview for the selected interactions, read in the contact's
// participation context. It writes nothing, so it is read as a query.
export function useMergePreview(interactionIds: string[], contactId: string) {
  return useQuery({
    queryKey: interactionKeys.mergePreview(contactId, interactionIds),
    queryFn: () =>
      interactionsApi.previewMerge({ interaction_ids: interactionIds, contact_id: contactId }),
    enabled: interactionIds.length >= 2 && !!contactId,
  })
}

export function useMergeInteractions() {
  return useMutation({
    mutationFn: ({ interactionIds, contactId }: { interactionIds: string[]; contactId: string }) =>
      interactionsApi.merge({ interaction_ids: interactionIds, contact_id: contactId }),
    onSuccess: () => invalidateFor('interaction:corrected'),
  })
}

export function useRemoveParticipations() {
  return useMutation({
    mutationFn: ({
      interactionId,
      body,
    }: {
      interactionId: string
      body: InteractionRemovalRequest
    }) => interactionsApi.removeParticipations(interactionId, body),
    onSuccess: () => invalidateFor('interaction:corrected'),
  })
}

export function useTimelineAssertions(identity: TimelineIdentityResponse, contactId: string) {
  return useQuery({
    queryKey: interactionKeys.timelineAssertions(contactId, identity.kind, identity.id),
    queryFn: () => interactionsApi.getTimelineAssertions(identity, contactId),
    enabled: !!identity.id && !!contactId,
    staleTime: staleTime(1000 * 60 * 5),
  })
}
