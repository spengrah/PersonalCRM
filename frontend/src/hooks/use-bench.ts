import { keepPreviousData, useMutation, useQueries, useQuery } from '@tanstack/react-query'
import { benchApi, type AllInteractionsFilters } from '@/lib/bench-api'
import { invalidateFor } from '@/lib/query-invalidation'
import { interactionKeys, judgmentKeys } from '@/lib/query-keys'
import type { ReviewJudgmentOutcomeRequest } from '@/types/generated/contact'

export function useAllInteractions(filters: AllInteractionsFilters) {
  return useQuery({
    queryKey: interactionKeys.allList(filters),
    queryFn: () => benchApi.list(filters),
    placeholderData: keepPreviousData,
  })
}

// The whole content of each judged thread.
export function useThreadContents(threadIds: string[]) {
  return useQueries({
    queries: threadIds.map(threadId => ({
      queryKey: interactionKeys.threadContent(threadId),
      queryFn: () => benchApi.threadContent(threadId),
    })),
  })
}

export function useJudgmentRuns() {
  return useQuery({
    queryKey: judgmentKeys.runs(),
    queryFn: () => benchApi.runs(),
  })
}

export function useReviewOutcome() {
  return useMutation({
    mutationFn: ({ outcomeId, body }: { outcomeId: string; body: ReviewJudgmentOutcomeRequest }) =>
      benchApi.review(outcomeId, body),
    onSuccess: () => invalidateFor('judgment:reviewed'),
  })
}
