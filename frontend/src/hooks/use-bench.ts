import { keepPreviousData, useMutation, useQueries, useQuery } from '@tanstack/react-query'
import type { ApiResponseWithMeta } from '@/lib/api-client'
import { benchApi, type AllInteractionsFilters } from '@/lib/bench-api'
import { queryClient } from '@/lib/query-client'
import { invalidateFor } from '@/lib/query-invalidation'
import { interactionKeys, judgmentKeys } from '@/lib/query-keys'
import type {
  AllInteractionsResponse,
  BenchProposalResponse,
  CloseBenchRowResponse,
  ReviewJudgmentOutcomeRequest,
} from '@/types/generated/contact'

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

/**
 * Every save on the bench runs in one queue, so a close of a row takes effect
 * after the saves made before it are recorded.
 */
const BENCH_SAVES = { id: 'bench-saves' }

/** A recorded close's row: the row it closed and what remains to review on it. */
interface ClosedRow {
  identityId: string
  status: Pick<CloseBenchRowResponse, 'to_review' | 'review_status'>
}

/**
 * Shows what a save recorded in every page of the view narrowed to the same
 * run, without waiting on the list read: each proposal replaces the one of
 * its judgment, and a closed row takes the status the server compiled. A list
 * read in flight from before the save is cancelled first, so it cannot land
 * over the recorded state; the caller's invalidation reads the list again.
 */
async function showRecorded(
  runId: string | undefined,
  proposals: BenchProposalResponse[],
  closed?: ClosedRow
) {
  await queryClient.cancelQueries({ queryKey: interactionKeys.allLists() })
  const recorded = new Map(proposals.map(proposal => [proposal.judgment_id, proposal]))
  queryClient.setQueriesData<ApiResponseWithMeta<AllInteractionsResponse>>(
    {
      queryKey: interactionKeys.allLists(),
      predicate: query =>
        (query.queryKey[2] as AllInteractionsFilters | undefined)?.run_id === runId,
    },
    page =>
      page?.data
        ? {
            ...page,
            data: {
              ...page.data,
              items: page.data.items.map(row => ({
                ...row,
                proposals: row.proposals.map(p => recorded.get(p.judgment_id) ?? p),
                ...(row.item.identity.id === closed?.identityId && closed.status),
              })),
            },
          }
        : page
  )
}

export function useReviewOutcome() {
  return useMutation({
    scope: BENCH_SAVES,
    mutationFn: ({
      outcomeId,
      body,
      runId,
    }: {
      outcomeId: string
      body: ReviewJudgmentOutcomeRequest
      runId?: string
    }) => benchApi.review(outcomeId, body, runId),
    onSuccess: async (recorded, { runId }) => {
      await showRecorded(runId, recorded.proposals)
      invalidateFor('judgment:reviewed')
    },
  })
}

// The list read is refreshed after a failure too, so the bench shows what the
// server recorded.
export function useConfirmNones() {
  return useMutation({
    scope: BENCH_SAVES,
    mutationFn: ({ judgmentId, runId }: { judgmentId: string; runId?: string }) =>
      benchApi.confirmNones(judgmentId, runId),
    onSuccess: (recorded, { runId }) => showRecorded(runId, recorded.proposals),
    onSettled: () => invalidateFor('judgment:nones-confirmed'),
  })
}

// A close is sent for every row the user collapses; the server decides what
// it labels. The list read is refreshed after a failure too.
export function useCloseRow() {
  return useMutation({
    scope: BENCH_SAVES,
    mutationFn: ({
      judgmentIds,
      runId,
    }: {
      identityId: string
      judgmentIds: string[]
      runId?: string
    }) => benchApi.closeRow(judgmentIds, runId),
    onSuccess: (recorded, { identityId, runId }) =>
      showRecorded(runId, recorded.proposals, {
        identityId,
        status: { to_review: recorded.to_review, review_status: recorded.review_status },
      }),
    onSettled: () => invalidateFor('judgment:row-closed'),
  })
}
