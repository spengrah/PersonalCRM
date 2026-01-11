import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { importsApi } from '@/lib/imports-api'
import { contactKeys, importKeys, invalidateFor } from '@/lib/query-invalidation'
import type {
  ImportCandidatesListParams,
  ImportContactRequest,
  LinkContactRequest,
} from '@/types/import'

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
// Supports optional method selection for enhanced UI
export function useImportAsContact() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ id, request }: { id: string; request?: ImportContactRequest }) =>
      importsApi.importCandidate(id, request),
    onSuccess: newContact => {
      // Populate the contact detail cache with the new contact
      queryClient.setQueryData(contactKeys.detail(newContact.id), newContact)
      invalidateFor('import:imported')
    },
  })
}

// Link candidate to existing CRM contact
// Supports method selection and conflict resolutions for enhanced UI
export function useLinkCandidate() {
  return useMutation({
    mutationFn: ({ id, request }: { id: string; request: LinkContactRequest }) =>
      importsApi.linkCandidate(id, request),
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
    mutationFn: ({ source = 'gcontacts', accountId }: { source?: string; accountId?: string }) =>
      importsApi.triggerSync(source, accountId),
    onSuccess: () => {
      invalidateFor('import:synced')
    },
  })
}
