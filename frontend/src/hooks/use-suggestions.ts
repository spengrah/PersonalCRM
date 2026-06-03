import { useMutation, useQuery } from '@tanstack/react-query'
import { importsApi } from '@/lib/imports-api'
import { importKeys, invalidateFor } from '@/lib/query-invalidation'
import { useRegisterRematchJob } from '@/components/providers/rematch-jobs-provider'
import type {
  SuggestionsListParams,
  ResolveMethodSuggestionsRequest,
  DismissMethodSuggestionsRequest,
} from '@/types/import'

// Get the unified suggestions list (method group + confidence-ranked
// candidates). Keyed by the filter/page params so variants cache-bust.
export function useSuggestions(params: SuggestionsListParams = {}) {
  return useQuery({
    queryKey: importKeys.suggestions(params),
    queryFn: () => importsApi.getSuggestions(params),
    staleTime: 1000 * 60 * 2, // 2 minutes
  })
}

// Confirm selected pending methods for an already-linked contact. On
// success the contact is enriched (rematch backfill registered) and the
// suggestions surface + contact detail refresh.
export function useResolveMethodSuggestions() {
  const registerJob = useRegisterRematchJob()

  return useMutation({
    mutationFn: ({ id, request }: { id: string; request: ResolveMethodSuggestionsRequest }) =>
      importsApi.resolveMethodSuggestions(id, request),
    onSuccess: response => {
      invalidateFor('method-suggestion:resolved', response.contact_id)
      if (response.rematch_job_id) {
        registerJob({
          jobId: response.rematch_job_id,
          contactId: response.contact_id,
          invalidateImports: true,
        })
      }
    },
  })
}

// Dismiss selected (or all) pending methods. No contact mutation, no
// rematch — only the suggestions surface refreshes.
export function useDismissMethodSuggestions() {
  return useMutation({
    mutationFn: ({ id, request }: { id: string; request: DismissMethodSuggestionsRequest }) =>
      importsApi.dismissMethodSuggestions(id, request),
    onSuccess: () => {
      invalidateFor('method-suggestion:dismissed')
    },
  })
}
