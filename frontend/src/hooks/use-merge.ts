import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { apiClient } from '@/lib/api-client'
import { contactKeys, invalidateFor } from '@/lib/query-invalidation'
import { noteKeys } from '@/hooks/use-contact-note'
import type { Contact } from '@/types/contact'

// Merge preview response from API
export interface MergePreview {
  source_contact: Contact
  target_contact: Contact
  methods_to_transfer: number
  duplicate_methods: number
  notes_to_transfer: number
  interactions_to_transfer: number
  calendar_events_to_update: number
}

// Field selections for merge
export interface MergeFieldSelections {
  cadence?: 'source' | 'target'
  location?: 'source' | 'target'
  birthday?: 'source' | 'target'
}

// Merge request
export interface MergeContactsRequest {
  source_contact_id: string
  field_selections?: MergeFieldSelections
  new_name?: string
}

// Query keys for merge
export const mergeKeys = {
  all: ['merge'] as const,
  preview: (targetId: string, sourceId: string) =>
    [...mergeKeys.all, 'preview', targetId, sourceId] as const,
}

// API functions
const mergeApi = {
  getMergePreview: async (targetId: string, sourceId: string): Promise<MergePreview> => {
    return apiClient.get<MergePreview>(
      `/api/v1/contacts/${targetId}/merge/preview?source_id=${sourceId}`
    )
  },

  mergeContacts: async (targetId: string, request: MergeContactsRequest): Promise<Contact> => {
    return apiClient.post<Contact>(`/api/v1/contacts/${targetId}/merge`, request)
  },
}

// Get merge preview
export function useMergePreview(targetId: string, sourceId: string) {
  return useQuery({
    queryKey: mergeKeys.preview(targetId, sourceId),
    queryFn: () => mergeApi.getMergePreview(targetId, sourceId),
    enabled: !!targetId && !!sourceId && targetId !== sourceId,
    staleTime: 1000 * 30, // 30 seconds - preview data can change
  })
}

// Merge contacts mutation
export function useMergeContacts() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ targetId, request }: { targetId: string; request: MergeContactsRequest }) =>
      mergeApi.mergeContacts(targetId, request),
    onSuccess: mergedContact => {
      // Update the merged contact in cache
      queryClient.setQueryData(contactKeys.detail(mergedContact.id), mergedContact)
      // Invalidate contact lists since source was deleted and target was updated
      invalidateFor('contact:merged')
      // Invalidate the merged contact's note query to fetch the combined note
      queryClient.invalidateQueries({ queryKey: noteKeys.contactNote(mergedContact.id) })
    },
  })
}
