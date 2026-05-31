import { apiClient } from './api-client'
import type {
  ImportCandidate,
  ImportCandidatesListParams,
  ImportCandidatesListResponse,
  ImportContactRequest,
  ImportContactResponse,
  LinkContactRequest,
  LinkContactResponse,
  NeedsAttentionItem,
  ResolveLinkRequest,
  ResolveLinkResponse,
  DiscoveryGroup,
  ResolveDiscoveryTokenRequest,
  ResolveDiscoveryTokenResponse,
} from '@/types/import'

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
  // Accepts optional method selection for enhanced UI. Returns the new
  // contact wrapped with an optional rematch_job_id (set when historical
  // calendar events / telegram messages may be retroactively linked).
  importCandidate: async (
    id: string,
    request?: ImportContactRequest
  ): Promise<ImportContactResponse> => {
    return apiClient.post<ImportContactResponse>(`/api/v1/imports/${id}/import`, request)
  },

  // Link candidate to existing CRM contact
  // Accepts method selection and conflict resolutions for enhanced UI.
  // Returns the linked external contact wrapped with an optional
  // rematch_job_id (set when newly-enriched methods may retroactively link
  // historical events / messages).
  linkCandidate: async (id: string, request: LinkContactRequest): Promise<LinkContactResponse> => {
    return apiClient.post<LinkContactResponse>(`/api/v1/imports/${id}/link`, request)
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

  // Interactions tab: conflict + orphan queue. The UI wants all hosts, so
  // no host_id is passed.
  getNeedsAttention: async (): Promise<NeedsAttentionItem[]> => {
    const data = await apiClient.get<NeedsAttentionItem[]>('/api/v1/meeting-notes/needs-attention')
    return data || []
  },

  // Resolve a conflict/orphan: link to a candidate (This one) or log as
  // impromptu (None of these / Log as impromptu).
  resolveLink: async (id: string, request: ResolveLinkRequest): Promise<ResolveLinkResponse> => {
    return apiClient.post<ResolveLinkResponse>(`/api/v1/meeting-notes/${id}/resolve-link`, request)
  },

  // People tab: grouped anarlog_title discovery tokens.
  getAnarlogTitleGroups: async (): Promise<DiscoveryGroup[]> => {
    const data = await apiClient.get<DiscoveryGroup[]>('/api/v1/imports/anarlog-title')
    return data || []
  },

  // Resolve a whole anarlog_title token group: import / link / ignore.
  resolveDiscoveryToken: async (
    request: ResolveDiscoveryTokenRequest
  ): Promise<ResolveDiscoveryTokenResponse> => {
    return apiClient.post<ResolveDiscoveryTokenResponse>(
      '/api/v1/imports/anarlog-title/resolve',
      request
    )
  },
}
