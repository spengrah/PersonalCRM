import { apiClient } from './api-client'
import type { 
  Contact, 
  CreateContactRequest, 
  UpdateContactRequest, 
  ContactListParams,
  OverdueContact
} from '@/types/contact'

export interface ContactsListResponse {
  contacts: Contact[]
  total: number
  page: number
  limit: number
  pages: number
}

export const contactsApi = {
  // Get all contacts
  getContacts: async (params: ContactListParams = {}): Promise<ContactsListResponse> => {
    const queryParams = {
      page: params.page || 1,
      limit: params.limit || 20,
      ...(params.search && { search: params.search }),
      ...(params.sort && { sort: params.sort }),
      ...(params.order && { order: params.order }),
    }
    
    // We need to make a raw request to get both data and meta
    const url = new URL('/api/v1/contacts', process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080')
    Object.entries(queryParams).forEach(([key, value]) => {
      if (value !== undefined) {
        url.searchParams.append(key, String(value))
      }
    })
    
    const response = await fetch(url.toString())
    if (!response.ok) {
      throw new Error(`HTTP error! status: ${response.status}`)
    }
    
    const result = await response.json()
    
    return {
      contacts: result.data || [],
      total: result.meta?.pagination?.total || 0,
      page: result.meta?.pagination?.page || 1,
      limit: result.meta?.pagination?.limit || 20,
      pages: result.meta?.pagination?.pages || 0,
    }
  },

  // Get single contact
  getContact: async (id: string): Promise<Contact> => {
    return apiClient.get<Contact>(`/api/v1/contacts/${id}`)
  },

  // Create contact
  createContact: async (data: CreateContactRequest): Promise<Contact> => {
    return apiClient.post<Contact>('/api/v1/contacts', data)
  },

  // Update contact
  updateContact: async (id: string, data: UpdateContactRequest): Promise<Contact> => {
    return apiClient.put<Contact>(`/api/v1/contacts/${id}`, data)
  },

  // Delete contact (soft delete)
  deleteContact: async (id: string): Promise<void> => {
    return apiClient.delete<void>(`/api/v1/contacts/${id}`)
  },

  // Update last contacted
  updateLastContacted: async (id: string): Promise<Contact> => {
    return apiClient.patch<Contact>(`/api/v1/contacts/${id}/last-contacted`)
  },

  // Get overdue contacts
  getOverdueContacts: async (): Promise<OverdueContact[]> => {
    return apiClient.get<OverdueContact[]>('/api/v1/contacts/overdue')
  },
}

