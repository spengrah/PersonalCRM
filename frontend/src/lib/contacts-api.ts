import { apiClient } from './api-client'
import type {
  Contact,
  CreateContactRequest,
  UpdateContactRequest,
  ContactListParams,
  OverdueContact,
} from '@/types/contact'

export interface ContactsListResponse {
  contacts: Contact[]
  total: number
  page: number
  limit: number
  pages: number
}

export interface ContactIDsResponse {
  ids: string[]
  total: number
}

export interface ContactIDsParams {
  sort?: string
  order?: 'asc' | 'desc'
  search?: string
  cadence_filter?: 'has_cadence' | 'no_cadence'
  followup_filter?: 'has_followup' | 'no_followup'
}

export interface UpdateLastContactedRequest {
  last_contacted?: string // ISO 8601 date string (YYYY-MM-DD)
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
      ...(params.cadence_filter && { cadence_filter: params.cadence_filter }),
      ...(params.followup_filter && { followup_filter: params.followup_filter }),
    }

    const response = await apiClient.getWithMeta<Contact[]>('/api/v1/contacts', queryParams)

    return {
      contacts: response.data || [],
      total: response.meta?.pagination?.total || 0,
      page: response.meta?.pagination?.page || 1,
      limit: response.meta?.pagination?.limit || 20,
      pages: response.meta?.pagination?.pages || 0,
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
  // If date is provided, sets last_contacted to that date
  // If date is omitted, sets last_contacted to current time
  updateLastContacted: async (id: string, date?: string): Promise<Contact> => {
    const body: UpdateLastContactedRequest = date ? { last_contacted: date } : {}
    return apiClient.patch<Contact>(`/api/v1/contacts/${id}/last-contacted`, body)
  },

  // Get overdue contacts
  getOverdueContacts: async (): Promise<OverdueContact[]> => {
    return apiClient.get<OverdueContact[]>('/api/v1/contacts/overdue')
  },

  // Get contact IDs only (lightweight, for navigation)
  getContactIDs: async (params: ContactIDsParams = {}): Promise<ContactIDsResponse> => {
    const queryParams = {
      ids_only: 'true',
      ...(params.sort && { sort: params.sort }),
      ...(params.order && { order: params.order }),
      ...(params.search && { search: params.search }),
      ...(params.cadence_filter && { cadence_filter: params.cadence_filter }),
      ...(params.followup_filter && { followup_filter: params.followup_filter }),
    }

    return apiClient.get<ContactIDsResponse>('/api/v1/contacts', queryParams)
  },
}
