import { apiClient } from './api-client'
import type { ApiResponseWithMeta } from './api-client'
import type {
  InteractionDirection,
  InteractionResponse,
  InteractionListResponse,
  InteractionContentResponse,
} from '@/types/generated/contact'

export type { InteractionDirection, InteractionResponse }

export interface InteractionListFilters {
  venue?: string
  from?: string
  to?: string
}

export interface InteractionListParams extends InteractionListFilters {
  page?: number
  limit?: number
}

export interface CreateInteractionRequest {
  direction: InteractionDirection
  // ISO 8601 timestamp. Omit to let the backend use the current
  // (accelerated) time — the safe default for "I just talked to them".
  occurred_at?: string
  description?: string
}

export const interactionsApi = {
  // Create a manual interaction for a contact. Backend defaults
  // direction-aware fields (last_contacted / last_outreach_at / last_response_at)
  // based on the direction value.
  async create(contactId: string, body: CreateInteractionRequest): Promise<InteractionResponse> {
    return apiClient.post<InteractionResponse>(`/api/v1/contacts/${contactId}/interactions`, body)
  },

  async list(
    contactId: string,
    params: InteractionListParams = {}
  ): Promise<ApiResponseWithMeta<InteractionListResponse>> {
    return apiClient.getWithMeta<InteractionListResponse>(
      `/api/v1/contacts/${contactId}/interactions`,
      params as Record<string, unknown>
    )
  },

  async getContent(interactionId: string): Promise<InteractionContentResponse> {
    return apiClient.get<InteractionContentResponse>(
      `/api/v1/interactions/${interactionId}/content`
    )
  },
}
