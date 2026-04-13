import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { importsApi } from '@/lib/imports-api'
import { contactKeys, importKeys, invalidateFor } from '@/lib/query-invalidation'
import { useRegisterRematchJob } from '@/components/providers/rematch-jobs-provider'
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
  const registerJob = useRegisterRematchJob()

  return useMutation({
    mutationFn: ({ id, request }: { id: string; request?: ImportContactRequest }) =>
      importsApi.importCandidate(id, request),
    onSuccess: response => {
      // Populate the contact detail cache with the new contact
      queryClient.setQueryData(contactKeys.detail(response.contact.id), response.contact)
      invalidateFor('import:imported')
      if (response.rematch_job_id) {
        registerJob({
          jobId: response.rematch_job_id,
          contactId: response.contact.id,
          invalidateImports: true,
        })
      }
    },
  })
}

// Link candidate to existing CRM contact
// Supports method selection and conflict resolutions for enhanced UI
export function useLinkCandidate() {
  const registerJob = useRegisterRematchJob()

  return useMutation({
    mutationFn: ({ id, request }: { id: string; request: LinkContactRequest }) =>
      importsApi.linkCandidate(id, request),
    onSuccess: (response, vars) => {
      invalidateFor('import:linked')
      if (response.rematch_job_id) {
        registerJob({
          jobId: response.rematch_job_id,
          contactId: vars.request.crm_contact_id,
          invalidateImports: true,
        })
      }
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

// Mutation key for sync operations (used to track pending syncs globally)
export const syncMutationKey = ['sync', 'trigger'] as const

// Trigger manual sync
export function useTriggerSync() {
  return useMutation({
    mutationKey: syncMutationKey,
    mutationFn: ({ source = 'gcontacts', accountId }: { source?: string; accountId?: string }) =>
      importsApi.triggerSync(source, accountId),
    onSuccess: () => {
      // Invalidate after sync completes to show final status
      invalidateFor('import:synced')
    },
  })
}
