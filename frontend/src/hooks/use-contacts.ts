import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { contactsApi, type ContactIDsParams } from '@/lib/contacts-api'
import { contactKeys, invalidateFor } from '@/lib/query-invalidation'
import type { CreateContactRequest, UpdateContactRequest, ContactListParams } from '@/types/contact'

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

  return useMutation({
    mutationFn: (data: CreateContactRequest) => contactsApi.createContact(data),
    onSuccess: newContact => {
      queryClient.setQueryData(contactKeys.detail(newContact.id), newContact)
      invalidateFor('contact:created')
    },
  })
}

// Update contact mutation
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

// Delete contact mutation
export function useDeleteContact() {
  return useMutation({
    mutationFn: (id: string) => contactsApi.deleteContact(id),
    onSuccess: () => {
      invalidateFor('contact:deleted')
    },
  })
}

// Update last contacted mutation
// Pass { id, date } where date is an optional YYYY-MM-DD string
// If date is omitted, sets last_contacted to current time
export function useUpdateLastContacted() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ id, date }: { id: string; date?: string }) =>
      contactsApi.updateLastContacted(id, date),
    onSuccess: updatedContact => {
      queryClient.setQueryData(contactKeys.detail(updatedContact.id), updatedContact)
      invalidateFor('contact:touched')
    },
  })
}
