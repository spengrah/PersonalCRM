import { apiClient } from './api-client'
import type { ApiResponseWithMeta } from './api-client'
import type {
  AllInteractionsResponse,
  BenchThreadContentResponse,
  InteractionTypeOptionResponse,
  JudgmentOutcomeResponse,
  JudgmentRunsResponse,
  ReviewJudgmentOutcomeRequest,
} from '@/types/generated/contact'

export type InteractionType = InteractionTypeOptionResponse['key']

/** The all-interactions view's filters; each is part of the page's address. */
export interface AllInteractionsFilters {
  contact_id?: string
  type?: InteractionType
  run_id?: string
  page?: number
}

export const ALL_INTERACTIONS_PAGE_SIZE = 20

export const benchApi = {
  // One page of every interaction across contacts, each row with the
  // proposals of the thread judgments it holds, compiled by the server.
  async list(
    filters: AllInteractionsFilters
  ): Promise<ApiResponseWithMeta<AllInteractionsResponse>> {
    return apiClient.getWithMeta<AllInteractionsResponse>('/api/v1/interactions', {
      ...filters,
      limit: ALL_INTERACTIONS_PAGE_SIZE,
    } as Record<string, unknown>)
  },

  // A judged thread's whole content, each message once, oldest first.
  async threadContent(threadId: string): Promise<BenchThreadContentResponse> {
    return apiClient.get<BenchThreadContentResponse>(
      `/api/v1/interactions/threads/${threadId}/content`
    )
  },

  async runs(): Promise<JudgmentRunsResponse> {
    return apiClient.get<JudgmentRunsResponse>('/api/v1/judgment-runs')
  },

  // Records the right outcome the user states; the server derives the
  // verdict, so a review never carries one.
  async review(
    outcomeId: string,
    body: ReviewJudgmentOutcomeRequest
  ): Promise<JudgmentOutcomeResponse> {
    return apiClient.post<JudgmentOutcomeResponse>(
      `/api/v1/judgment-outcomes/${outcomeId}/review`,
      body
    )
  },

  // Labels every negative decision of a proposal that has no label with its
  // judged value; the server decides which, and refuses a proposal with an item.
  async confirmNones(judgmentId: string): Promise<void> {
    await apiClient.post<void>(`/api/v1/judgments/${judgmentId}/confirm-nones`)
  },
}
