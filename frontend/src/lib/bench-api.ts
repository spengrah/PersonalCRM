import { apiClient } from './api-client'
import type { ApiResponseWithMeta } from './api-client'
import type {
  AllInteractionsResponse,
  ClearingLabelRequest,
  FlagLabelRequest,
  InteractionTypeOptionResponse,
  ReviewJudgmentOutcomeRequest,
  ReviewJudgmentOutcomeResponse,
  ReviewRunsResponse,
  ThreadListResponse,
  ThreadReadResponse,
  ThreadReviewResponse,
} from '@/types/generated/contact'

export type InteractionType = InteractionTypeOptionResponse['key']

/** The Interactions tab's filters; each is part of the tab's address. */
export interface AllInteractionsFilters {
  contact_id?: string
  type?: InteractionType
  page?: number
}

export const ALL_INTERACTIONS_PAGE_SIZE = 20

/** What narrows a thread list, and the list a thread page walks. */
export interface ThreadFilters {
  type?: InteractionType
  contact_id?: string
  run_id?: string
}

/** The Threads list's filters; each is part of the tab's address. */
export interface ThreadListFilters extends ThreadFilters {
  page?: number
}

export const THREAD_PAGE_SIZE = 20

/** A save's path, naming the run whose review state the answer carries. */
function inRun(path: string, runId: string): string {
  return `${path}?run_id=${encodeURIComponent(runId)}`
}

export const benchApi = {
  // One page of every interaction across contacts.
  async list(
    filters: AllInteractionsFilters
  ): Promise<ApiResponseWithMeta<AllInteractionsResponse>> {
    return apiClient.getWithMeta<AllInteractionsResponse>('/api/v1/interactions', {
      ...filters,
      limit: ALL_INTERACTIONS_PAGE_SIZE,
    } as Record<string, unknown>)
  },
}

export const threadsApi = {
  // One page of the threads, newest latest message first; given a run, the
  // threads it judged with their judged count and status, and the run's
  // summary.
  async list(filters: ThreadListFilters): Promise<ApiResponseWithMeta<ThreadListResponse>> {
    return apiClient.getWithMeta<ThreadListResponse>('/api/v1/threads', {
      ...filters,
      limit: THREAD_PAGE_SIZE,
    } as Record<string, unknown>)
  },

  // A thread, its place in the list the filters keep and, given a run, the
  // run's review of it.
  async read(threadId: string, filters: ThreadFilters): Promise<ThreadReadResponse> {
    return apiClient.get<ThreadReadResponse>(
      `/api/v1/threads/${threadId}`,
      filters as Record<string, unknown>
    )
  },

  async runs(): Promise<ReviewRunsResponse> {
    return apiClient.get<ReviewRunsResponse>('/api/v1/review-runs')
  },

  // Every save below answers with the thread's review state in the run, as a
  // read of the thread returns it.
  async labelFlag(
    threadId: string,
    runId: string,
    label: FlagLabelRequest
  ): Promise<ThreadReviewResponse> {
    return apiClient.post<ThreadReviewResponse>(
      inRun(`/api/v1/threads/${threadId}/flag-labels`, runId),
      label
    )
  },

  async labelClearing(
    threadId: string,
    runId: string,
    label: ClearingLabelRequest
  ): Promise<ThreadReviewResponse> {
    return apiClient.post<ThreadReviewResponse>(
      inRun(`/api/v1/threads/${threadId}/clearing-labels`, runId),
      label
    )
  },

  // Records the right outcome the user states for a reply item; the server
  // derives the verdict, so a review never carries one.
  async reviewReplyItem(
    outcomeId: string,
    runId: string,
    body: ReviewJudgmentOutcomeRequest
  ): Promise<ReviewJudgmentOutcomeResponse> {
    return apiClient.post<ReviewJudgmentOutcomeResponse>(
      inRun(`/api/v1/judgment-outcomes/${outcomeId}/review`, runId),
      body
    )
  },

  // Labels a night that shows nothing; the server refuses a night that shows
  // anything.
  async confirmNones(judgmentId: string, runId: string): Promise<ThreadReviewResponse> {
    return apiClient.post<ThreadReviewResponse>(
      inRun(`/api/v1/judgments/${judgmentId}/confirm-nones`, runId)
    )
  },

  // Records the user closing a thread; the server labels what a close labels.
  async close(threadId: string, runId: string): Promise<ThreadReviewResponse> {
    return apiClient.post<ThreadReviewResponse>(inRun(`/api/v1/threads/${threadId}/close`, runId))
  },
}
