import { keepPreviousData, useMutation, useQueries, useQuery } from '@tanstack/react-query'
import type { ApiResponseWithMeta } from '@/lib/api-client'
import { benchApi, type AllInteractionsFilters } from '@/lib/bench-api'
import { queryClient } from '@/lib/query-client'
import { invalidateFor } from '@/lib/query-invalidation'
import { interactionKeys, judgmentKeys } from '@/lib/query-keys'
import type {
  AllInteractionsResponse,
  BenchProposalResponse,
  BenchRowRequest,
  BenchRowStateResponse,
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

type ListPage = ApiResponseWithMeta<AllInteractionsResponse>

/** The cached pages of the view narrowed to the run. */
function runPages(runId: string | undefined) {
  return {
    queryKey: interactionKeys.allLists(),
    predicate: (query: { queryKey: readonly unknown[] }) =>
      (query.queryKey[2] as AllInteractionsFilters | undefined)?.run_id === runId,
  }
}

/** A row the bench shows: its identity and the judgments of its proposals. */
interface ShownRow {
  identityId: string
  judgmentIds: string[]
}

/**
 * The rows, across the cached pages of the view narrowed to the run, that
 * show a proposal of one of the threads: the rows a save on them can change.
 */
function rowsShowing(runId: string | undefined, threadIds: string[]): ShownRow[] {
  const rows = new Map<string, ShownRow>()
  for (const [, page] of queryClient.getQueriesData<ListPage>(runPages(runId))) {
    for (const row of page?.data?.items ?? []) {
      if (row.proposals.some(proposal => threadIds.includes(proposal.thread_id))) {
        rows.set(row.item.identity.id, {
          identityId: row.item.identity.id,
          judgmentIds: row.proposals.map(proposal => proposal.judgment_id),
        })
      }
    }
  }
  return [...rows.values()]
}

/**
 * Sends a save on the threads with the rows it can change, then shows each
 * of those rows as the server returned it, proposals, count and status
 * together, in every page of the view narrowed to the same run, without
 * waiting on the list read. A list read in flight from before the save is
 * cancelled first, so it cannot land over the recorded state; the caller's
 * invalidation reads the list again.
 */
async function save<T extends { rows: BenchRowStateResponse[] }>(
  runId: string | undefined,
  threadIds: string[],
  send: (rows: BenchRowRequest[]) => Promise<T>
): Promise<T> {
  const shown = rowsShowing(runId, threadIds)
  const recorded = await send(shown.map(row => ({ judgment_ids: row.judgmentIds })))
  await queryClient.cancelQueries({ queryKey: interactionKeys.allLists() })
  const rows = new Map(shown.map((row, i) => [row.identityId, recorded.rows[i]]))
  queryClient.setQueriesData<ListPage>(runPages(runId), page =>
    page?.data
      ? {
          ...page,
          data: {
            ...page.data,
            items: page.data.items.map(row => {
              const state = rows.get(row.item.identity.id)
              return state
                ? {
                    ...row,
                    proposals: state.proposals,
                    to_review: state.to_review,
                    review_status: state.review_status,
                  }
                : row
            }),
          },
        }
      : page
  )
  return recorded
}

export function useReviewOutcome() {
  return useMutation({
    scope: BENCH_SAVES,
    mutationFn: ({
      outcomeId,
      threadId,
      body,
      runId,
    }: {
      outcomeId: string
      threadId: string
      body: ReviewJudgmentOutcomeRequest
      runId?: string
    }) => save(runId, [threadId], rows => benchApi.review(outcomeId, { ...body, rows }, runId)),
    onSuccess: () => invalidateFor('judgment:reviewed'),
  })
}

// The list read is refreshed after a failure too, so the bench shows what the
// server recorded.
export function useConfirmNones() {
  return useMutation({
    scope: BENCH_SAVES,
    mutationFn: ({
      judgmentId,
      threadId,
      runId,
    }: {
      judgmentId: string
      threadId: string
      runId?: string
    }) => save(runId, [threadId], rows => benchApi.confirmNones(judgmentId, rows, runId)),
    onSettled: () => invalidateFor('judgment:nones-confirmed'),
  })
}

// A close is sent for every row the user collapses; the server decides what
// it labels. The list read is refreshed after a failure too.
export function useCloseRow() {
  return useMutation({
    scope: BENCH_SAVES,
    mutationFn: ({ proposals, runId }: { proposals: BenchProposalResponse[]; runId?: string }) =>
      save(
        runId,
        proposals.map(proposal => proposal.thread_id),
        rows =>
          benchApi.closeRow(
            proposals.map(proposal => proposal.judgment_id),
            rows,
            runId
          )
      ),
    onSettled: () => invalidateFor('judgment:row-closed'),
  })
}
