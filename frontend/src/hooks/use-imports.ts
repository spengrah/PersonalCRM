import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { importsApi } from '@/lib/imports-api'
import { contactKeys, importKeys, invalidateFor } from '@/lib/query-invalidation'
import type { ImportCandidatesListParams } from '@/types/import'

// Re-export importKeys for backward compatibility
export { importKeys }

// Get import candidates list
export function useImportCandidates(params: ImportCandidatesListParams = {}) {
  return useQuery({
    queryKey: importKeys.list(params),
    queryFn: () => importsApi.getCandidates(params),
    staleTime: 1000 * 60 * 2, // 2 minutes
  })
}

// Get single import candidate
export function useImportCandidate(id: string) {
  return useQuery({
    queryKey: importKeys.detail(id),
    queryFn: () => importsApi.getCandidate(id),
    enabled: !!id,
  })
}

// Import candidate as new CRM contact
export function useImportAsContact() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => importsApi.importCandidate(id),
    onSuccess: newContact => {
      // Populate the contact detail cache with the new contact
      queryClient.setQueryData(contactKeys.detail(newContact.id), newContact)
      invalidateFor('import:imported')
    },
  })
}

// Link candidate to existing CRM contact
export function useLinkCandidate() {
  return useMutation({
    mutationFn: ({ id, crmContactId }: { id: string; crmContactId: string }) =>
      importsApi.linkCandidate(id, crmContactId),
    onSuccess: () => {
      invalidateFor('import:linked')
    },
  })
}

// Ignore candidate
export function useIgnoreCandidate() {
  return useMutation({
    mutationFn: (id: string) => importsApi.ignoreCandidate(id),
    onSuccess: () => {
      invalidateFor('import:ignored')
    },
  })
}

// Trigger manual sync
export function useTriggerSync() {
  return useMutation({
    mutationFn: (source: string = 'gcontacts') => importsApi.triggerSync(source),
    onSuccess: () => {
      invalidateFor('import:synced')
    },
  })
}
