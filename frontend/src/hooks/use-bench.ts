import {
  keepPreviousData,
  useMutation,
  useMutationState,
  useQuery,
  type MutateOptions,
} from '@tanstack/react-query'
import {
  benchApi,
  threadsApi,
  type AllInteractionsFilters,
  type ThreadFilters,
  type ThreadListFilters,
} from '@/lib/bench-api'
import { queryClient } from '@/lib/query-client'
import { invalidateFor, type DomainEvent } from '@/lib/query-invalidation'
import { interactionKeys, threadKeys } from '@/lib/query-keys'
import type {
  ReviewJudgmentOutcomeRequest,
  ThreadReadResponse,
  ThreadReviewResponse,
} from '@/types/generated/contact'
import type { FlagKind } from '@/types/generated/judgment'

export function useAllInteractions(filters: AllInteractionsFilters) {
  return useQuery({
    queryKey: interactionKeys.allList(filters),
    queryFn: () => benchApi.list(filters),
    placeholderData: keepPreviousData,
  })
}

// The Threads list; a run narrows it to the threads the run judged.
export function useThreads(filters: ThreadListFilters, enabled = true) {
  return useQuery({
    queryKey: threadKeys.list(filters),
    queryFn: () => threadsApi.list(filters),
    placeholderData: keepPreviousData,
    enabled,
  })
}

// A thread, its place in the list the filters keep and, with a run in the
// filters, the run's review of it.
export function useThread(threadId: string, filters: ThreadFilters) {
  return useQuery({
    queryKey: threadKeys.read(threadId, filters),
    queryFn: () => threadsApi.read(threadId, filters),
  })
}

// The runs the user reviews, newest first.
export function useReviewRuns() {
  return useQuery({
    queryKey: threadKeys.runs(),
    queryFn: () => threadsApi.runs(),
  })
}

/**
 * Every Threads save (a flag label, a clearing label, a reply-item review,
 * Confirm Nones, a close) runs through this one queue, so saves are sent in
 * the order made and a close is sent after every save made before it is
 * recorded. A queued save never blocks a click: the caller learns of its end
 * through the answer it draws, and navigating away does not wait for it.
 */
const THREAD_SAVES = { id: 'thread-saves' }
const THREAD_SAVE = ['thread-save'] as const

/** The controls a save came from; only those wait while it is pending. */
export const saveItem = {
  flag: (messageKey: string, kind: FlagKind) => `flag:${messageKey}:${kind}`,
  clearing: (commitmentMessageKey: string, cutOff: string) =>
    `clearing:${commitmentMessageKey}:${cutOff}`,
  reply: (outcomeId: string) => `reply:${outcomeId}`,
  nones: (judgmentId: string) => `nones:${judgmentId}`,
  close: (threadId: string) => `close:${threadId}`,
}

/** The thread and run a save names, whose review state its answer carries. */
interface SaveTarget {
  threadId: string
  runId: string
}

interface QueuedSave<V> {
  vars: V
  item: string
}

/**
 * Whether the controls of a save item wait on a pending save (one in flight or
 * queued behind another).
 */
export function usePendingSaves(): (item: string) => boolean {
  const items = useMutationState({
    filters: { mutationKey: THREAD_SAVE, status: 'pending' },
    select: mutation => (mutation.state.variables as QueuedSave<unknown>).item,
  })
  return item => items.includes(item)
}

/**
 * Shows the review state a save's answer carries, as the server computed it,
 * on every cached read of the thread in the same run. A read in flight from
 * before the save is cancelled first, so it cannot land over the recorded
 * state; the save's invalidation reads the thread again after it.
 */
async function showRecorded(target: SaveTarget, review: ThreadReviewResponse | undefined) {
  const reads = threadKeys.forThread(target.threadId)
  await queryClient.cancelQueries({ queryKey: reads })
  if (!review) return
  queryClient.setQueriesData<ThreadReadResponse>(
    {
      queryKey: reads,
      predicate: query => (query.queryKey[3] as ThreadFilters).run_id === target.runId,
    },
    read => read && { ...read, review }
  )
}

function useThreadSave<V extends SaveTarget>(
  itemOf: (vars: V) => string,
  send: (vars: V) => Promise<ThreadReviewResponse | undefined>,
  event: DomainEvent
) {
  const save = useMutation({
    mutationKey: THREAD_SAVE,
    scope: THREAD_SAVES,
    mutationFn: async ({ vars }: QueuedSave<V>) => {
      const review = await send(vars)
      await showRecorded(vars, review)
      return review
    },
    onSettled: () => invalidateFor(event),
  })
  return {
    mutate: (
      vars: V,
      options?: MutateOptions<ThreadReviewResponse | undefined, Error, QueuedSave<V>>
    ) => save.mutate({ vars, item: itemOf(vars) }, options),
  }
}

export interface LabelFlagVars extends SaveTarget {
  messageKey: string
  kind: FlagKind
  holds: boolean
}

export function useLabelFlag() {
  return useThreadSave(
    (v: LabelFlagVars) => saveItem.flag(v.messageKey, v.kind),
    v =>
      threadsApi.labelFlag(v.threadId, v.runId, {
        message_key: v.messageKey,
        kind: v.kind,
        holds: v.holds,
      }),
    'thread:labeled'
  )
}

export interface LabelClearingVars extends SaveTarget {
  commitmentMessageKey: string
  cutOff: string
  done: boolean
}

export function useLabelClearing() {
  return useThreadSave(
    (v: LabelClearingVars) => saveItem.clearing(v.commitmentMessageKey, v.cutOff),
    v =>
      threadsApi.labelClearing(v.threadId, v.runId, {
        commitment_message_key: v.commitmentMessageKey,
        cut_off: v.cutOff,
        done: v.done,
      }),
    'thread:labeled'
  )
}

export interface ReviewReplyItemVars extends SaveTarget {
  outcomeId: string
  review: ReviewJudgmentOutcomeRequest
}

// A reply item's review gives the thread the right outcome's effect on a first
// review, so it also reads the contacts and tasks it can change.
export function useReviewReplyItem() {
  return useThreadSave(
    (v: ReviewReplyItemVars) => saveItem.reply(v.outcomeId),
    async v => (await threadsApi.reviewReplyItem(v.outcomeId, v.runId, v.review)).thread_review,
    'thread:reply-reviewed'
  )
}

export interface ConfirmNonesVars extends SaveTarget {
  judgmentId: string
}

export function useConfirmNones() {
  return useThreadSave(
    (v: ConfirmNonesVars) => saveItem.nones(v.judgmentId),
    v => threadsApi.confirmNones(v.judgmentId, v.runId),
    'thread:nones-confirmed'
  )
}

// Done and Next close a thread; the server decides what a close labels, after
// every save made before it.
export function useCloseThread() {
  return useThreadSave(
    (v: SaveTarget) => saveItem.close(v.threadId),
    v => threadsApi.close(v.threadId, v.runId),
    'thread:closed'
  )
}
