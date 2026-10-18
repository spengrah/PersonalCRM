import { apiClient } from './api-client'
import type { ApiResponseWithMeta } from './api-client'
import type {
  AllInteractionsResponse,
  BenchRowRequest,
  BenchRowsResponse,
  BenchThreadContentResponse,
  InteractionTypeOptionResponse,
  JudgmentRunsResponse,
  ReviewJudgmentOutcomeRequest,
  ReviewJudgmentOutcomeResponse,
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

/** A save's path, carrying the run the view is narrowed to, so the rows it returns read as the view shows them. */
function inRun(path: string, runId: string | undefined): string {
  return runId ? `${path}?run_id=${encodeURIComponent(runId)}` : path
}

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
  // verdict, so a review never carries one. It returns the rows the body
  // names as the server recorded them.
  async review(
    outcomeId: string,
    body: ReviewJudgmentOutcomeRequest,
    runId?: string
  ): Promise<ReviewJudgmentOutcomeResponse> {
    return apiClient.post<ReviewJudgmentOutcomeResponse>(
      inRun(`/api/v1/judgment-outcomes/${outcomeId}/review`, runId),
      body
    )
  },

  // Labels every negative decision of a proposal that has no label with its
  // judged value; the server decides which, and refuses a proposal with an
  // item. It returns the rows named as the server recorded them.
  async confirmNones(
    judgmentId: string,
    rows: BenchRowRequest[],
    runId?: string
  ): Promise<BenchRowsResponse> {
    return apiClient.post<BenchRowsResponse>(
      inRun(`/api/v1/judgments/${judgmentId}/confirm-nones`, runId),
      { rows }
    )
  },

  // Records the user closing a row: the server labels what a close labels and
  // returns the rows named as it recorded them.
  async closeRow(
    judgmentIds: string[],
    rows: BenchRowRequest[],
    runId?: string
  ): Promise<BenchRowsResponse> {
    return apiClient.post<BenchRowsResponse>(inRun('/api/v1/judgments/close', runId), {
      judgment_ids: judgmentIds,
      rows,
    })
  },
}
