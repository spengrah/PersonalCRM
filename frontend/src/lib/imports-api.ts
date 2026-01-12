import { apiClient } from './api-client'
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

    const response = await apiClient.getWithMeta<ImportCandidate[]>(
      '/api/v1/imports/candidates',
      queryParams
    )

    return {
      candidates: response.data || [],
      total: response.meta?.pagination?.total || 0,
      page: response.meta?.pagination?.page || 1,
      limit: response.meta?.pagination?.limit || 20,
      pages: response.meta?.pagination?.pages || 0,
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
