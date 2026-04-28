import { apiClient } from './api-client'

export type InteractionDirection = 'inbound' | 'outbound' | 'mutual'

export interface CreateInteractionRequest {
  direction: InteractionDirection
  // ISO 8601 timestamp. Omit to let the backend use the current
  // (accelerated) time — the safe default for "I just talked to them".
  occurred_at?: string
  description?: string
}

export interface InteractionResponse {
  id: string
  contact_id: string
  source: string
  direction: InteractionDirection
  occurred_at: string
  description?: string
}

export const interactionsApi = {
  // Create a manual interaction for a contact. Backend defaults
  // direction-aware fields (last_contacted / last_outreach_at / last_response_at)
  // based on the direction value.
  async create(contactId: string, body: CreateInteractionRequest): Promise<InteractionResponse> {
    return apiClient.post<InteractionResponse>(`/api/v1/contacts/${contactId}/interactions`, body)
  },
}
