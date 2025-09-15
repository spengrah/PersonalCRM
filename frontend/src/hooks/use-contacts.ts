import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { contactsApi } from '@/lib/contacts-api'
import type { 
  CreateContactRequest, 
  UpdateContactRequest, 
  ContactListParams,
  OverdueContact
} from '@/types/contact'

// Query keys
export const contactKeys = {
  all: ['contacts'] as const,
  lists: () => [...contactKeys.all, 'list'] as const,
  list: (params: ContactListParams) => [...contactKeys.lists(), params] as const,
  details: () => [...contactKeys.all, 'detail'] as const,
  detail: (id: string) => [...contactKeys.details(), id] as const,
  overdue: () => [...contactKeys.all, 'overdue'] as const,
}

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
    queryFn: async () => {
      console.log('🔄 Fetching overdue contacts...')
      try {
        const result = await contactsApi.getOverdueContacts()
        console.log('✅ Overdue contacts fetched:', result?.length || 0, 'contacts')
        return result
      } catch (error) {
        console.error('❌ Failed to fetch overdue contacts:', error)
        throw error
      }
    },
    staleTime: 1000 * 60 * 5, // 5 minutes - match query client default
    refetchInterval: 1000 * 60 * 5, // Refetch every 5 minutes
    retry: 3, // Explicit retry count
    retryDelay: (attemptIndex) => Math.min(1000 * 2 ** attemptIndex, 30000), // Exponential backoff
  })
}

// Create contact mutation
export function useCreateContact() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (data: CreateContactRequest) => contactsApi.createContact(data),
    onSuccess: () => {
      // Invalidate and refetch contacts list
      queryClient.invalidateQueries({ queryKey: contactKeys.lists() })
    },
  })
}

// Update contact mutation
export function useUpdateContact() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: ({ id, data }: { id: string; data: UpdateContactRequest }) =>
      contactsApi.updateContact(id, data),
    onSuccess: (updatedContact) => {
      // Update the contact in cache
      queryClient.setQueryData(
        contactKeys.detail(updatedContact.id),
        updatedContact
      )
      // Invalidate lists to refresh
      queryClient.invalidateQueries({ queryKey: contactKeys.lists() })
    },
  })
}

// Delete contact mutation
export function useDeleteContact() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => contactsApi.deleteContact(id),
    onSuccess: () => {
      // Invalidate and refetch contacts list
      queryClient.invalidateQueries({ queryKey: contactKeys.lists() })
    },
  })
}

// Update last contacted mutation
export function useUpdateLastContacted() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: (id: string) => contactsApi.updateLastContacted(id),
    onSuccess: (updatedContact) => {
      // Update the contact in cache
      queryClient.setQueryData(
        contactKeys.detail(updatedContact.id),
        updatedContact
      )
      // Invalidate all related queries to refresh
      queryClient.invalidateQueries({ queryKey: contactKeys.lists() })
      queryClient.invalidateQueries({ queryKey: contactKeys.overdue() })
    },
  })
}
