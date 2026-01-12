import { apiClient, ApiError } from './api-client'
import type {
  ImportCandidate,
  ImportCandidatesListParams,
  ImportCandidatesListResponse,
  ImportContactRequest,
  LinkContactRequest,
} from '@/types/import'
import type { Contact } from '@/types/contact'

export const importsApi = {
  // Get import candidates (paginated)
  getCandidates: async (
    params: ImportCandidatesListParams = {}
  ): Promise<ImportCandidatesListResponse> => {
    const queryParams = {
      page: params.page || 1,
      limit: params.limit || 20,
      ...(params.source && { source: params.source }),
    }

    // Use raw fetch for pagination metadata (same pattern as contacts-api)
    const API_BASE_URL =
      process.env.NEXT_PUBLIC_API_URL ||
      (typeof window !== 'undefined' ? window.location.origin : '')
    const url = new URL('/api/v1/imports/candidates', API_BASE_URL)
    Object.entries(queryParams).forEach(([key, value]) => {
      if (value !== undefined) {
        url.searchParams.append(key, String(value))
      }
    })

    const response = await fetch(url.toString(), {
      headers: {
        'Content-Type': 'application/json',
        'X-API-Key': process.env.NEXT_PUBLIC_API_KEY || '',
      },
    })
    if (!response.ok) {
      let errorMessage = `HTTP ${response.status}: ${response.statusText}`
      let errorCode = 'UNKNOWN_ERROR'

      try {
        const errorData = await response.json()
        if (errorData.error) {
          errorMessage = errorData.error.message
          errorCode = errorData.error.code
        }
      } catch {
        // If we can't parse the error response, use the default message
      }

      throw new ApiError(errorMessage, response.status, errorCode)
    }

    const result = await response.json()

    const total = result.meta?.pagination?.total || 0
    const limit = result.meta?.pagination?.limit || 20
    const pages = Math.ceil(total / limit)

    return {
      candidates: result.data || [],
      total,
      page: result.meta?.pagination?.page || 1,
      limit,
      pages,
    }
  },

  // Get single import candidate
  getCandidate: async (id: string): Promise<ImportCandidate> => {
    return apiClient.get<ImportCandidate>(`/api/v1/imports/${id}`)
  },

  // Import candidate as new CRM contact
  // Accepts optional method selection for enhanced UI
  importCandidate: async (id: string, request?: ImportContactRequest): Promise<Contact> => {
    return apiClient.post<Contact>(`/api/v1/imports/${id}/import`, request)
  },

  // Link candidate to existing CRM contact
  // Accepts method selection and conflict resolutions for enhanced UI
  linkCandidate: async (id: string, request: LinkContactRequest): Promise<void> => {
    return apiClient.post<void>(`/api/v1/imports/${id}/link`, request)
  },

  // Ignore candidate (won't appear in list anymore)
  ignoreCandidate: async (id: string): Promise<void> => {
    return apiClient.post<void>(`/api/v1/imports/${id}/ignore`)
  },

  // Trigger manual sync for a source
  triggerSync: async (source: string = 'gcontacts', accountId?: string): Promise<void> => {
    return apiClient.post<void>(`/api/v1/sync/${source}/trigger`, {
      account_id: accountId,
    })
  },
}
