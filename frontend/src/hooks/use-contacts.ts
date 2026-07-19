import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { contactsApi, type ContactIDsParams } from '@/lib/contacts-api'
import { contactKeys, invalidateFor } from '@/lib/query-invalidation'
import { useRegisterRematchJob } from '@/components/providers/rematch-jobs-provider'
import type {
  Contact,
  CreateContactRequest,
  UpdateContactRequest,
  ContactListParams,
} from '@/types/contact'
import type { ContactMethodOperation } from '@/types/generated/contact'

// Re-export contactKeys for backward compatibility
export { contactKeys }

// Get contacts list
export function useContacts(params: ContactListParams = {}) {
  return useQuery({
    queryKey: contactKeys.list(params),
    queryFn: () => contactsApi.getContacts(params),
    staleTime: 1000 * 60 * 2, // 2 minutes
  })
}

// Get single contact
export function useContact(id: string) {
  return useQuery({
    queryKey: contactKeys.detail(id),
    queryFn: () => contactsApi.getContact(id),
    enabled: !!id,
  })
}

// Prefetch a contact (for navigation smoothness)
export function usePrefetchContact() {
  const queryClient = useQueryClient()

  return (id: string) => {
    if (!id) return
    queryClient.prefetchQuery({
      queryKey: contactKeys.detail(id),
      queryFn: () => contactsApi.getContact(id),
      staleTime: 1000 * 60 * 2, // 2 minutes
    })
  }
}

// Get overdue contacts
export function useOverdueContacts() {
  return useQuery({
    queryKey: contactKeys.overdue(),
    queryFn: () => contactsApi.getOverdueContacts(),
    staleTime: 1000 * 60 * 5, // 5 minutes
    refetchOnWindowFocus: true,
    // No refetchInterval needed - invalidation handles updates after mutations
  })
}

// Get contact IDs only (lightweight, for navigation)
export function useContactIDs(params: ContactIDsParams = {}) {
  return useQuery({
    queryKey: [...contactKeys.lists(), 'navigation-ids', params],
    queryFn: () => contactsApi.getContactIDs(params),
    staleTime: 1000 * 60 * 5, // 5 minutes - navigation list doesn't need to be super fresh
  })
}

// Create contact mutation
export function useCreateContact() {
  const queryClient = useQueryClient()
  const registerJob = useRegisterRematchJob()

  return useMutation({
    mutationFn: (data: CreateContactRequest) => contactsApi.createContact(data),
    onSuccess: newContact => {
      queryClient.setQueryData(contactKeys.detail(newContact.id), newContact)
      invalidateFor('contact:created')
      if (newContact.rematch_job_id) {
        registerJob({ jobId: newContact.rematch_job_id, contactId: newContact.id })
      }
    },
  })
}

// Update contact mutation.
//
// No rematch registration here: a rematch is triggered by newly-present method
// values, and this request no longer carries methods at all. That job is minted
// by the operations endpoint, so useApplyMethodOperations registers it.
export function useUpdateContact() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: UpdateContactRequest }) =>
      contactsApi.updateContact(id, data),
    onSuccess: updatedContact => {
      queryClient.setQueryData(contactKeys.detail(updatedContact.id), updatedContact)
      invalidateFor('contact:updated')
    },
  })
}

// Apply contact-method operations.
//
// Owns rematch registration for the edit path. The detail cache is refreshed
// from the response so the detail view shows the saved methods; that cache
// feeds DISPLAY only and must never feed operation derivation — the caller's
// acknowledged state is a separate, explicitly held value for exactly that
// reason.
export function useApplyMethodOperations() {
  const queryClient = useQueryClient()
  const registerJob = useRegisterRematchJob()

  return useMutation({
    mutationFn: ({ id, operations }: { id: string; operations: ContactMethodOperation[] }) =>
      contactsApi.applyMethodOperations(id, operations),
    onSuccess: (response, { id }) => {
      queryClient.setQueryData<Contact>(contactKeys.detail(id), previous =>
        previous ? { ...previous, methods: response.methods } : previous
      )
      invalidateFor('contact:updated')
      if (response.rematch_job_id) {
        registerJob({ jobId: response.rematch_job_id, contactId: id })
      }
    },
  })
}

// Delete contact mutation
export function useDeleteContact() {
  return useMutation({
    mutationFn: (id: string) => contactsApi.deleteContact(id),
    onSuccess: () => {
      invalidateFor('contact:deleted')
    },
  })
}
