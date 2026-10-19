import { useSyncExternalStore } from 'react'
import { keepPreviousData, MutationObserver, useQuery } from '@tanstack/react-query'
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
 *
 * The queue lives outside any component. A save that fails after the client's
 * retries stays in it, with the thread and run it names, and parks its
 * thread: the saves made after it wait, so a close never infers labels from a
 * decision the server did not record. Other threads' saves go on. The user
 * retries the failed save, which sends it and then the saves behind it, or
 * discards it, which drops it and the thread's closes behind it.
 */
type SaveStatus = 'queued' | 'running' | 'failed'

interface SaveEntry {
  id: number
  threadId: string
  runId: string
  /** The controls the save came from; only those wait while it is pending. */
  item: string
  /** What the user sees named when the save fails. */
  what: string
  closes: boolean
  status: SaveStatus
  send: () => Promise<void>
  event: DomainEvent
  onSuccess?: () => void
  onError?: (error: Error) => void
}

let saves: readonly SaveEntry[] = []
let nextId = 0
let sending = false
const listeners = new Set<() => void>()

function setSaves(next: readonly SaveEntry[]) {
  saves = next
  listeners.forEach(listener => listener())
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

// The queue is empty when a page renders on the server.
const NO_SAVES: readonly SaveEntry[] = []
const useSaves = () =>
  useSyncExternalStore(
    subscribe,
    () => saves,
    () => NO_SAVES
  )

const parked = (threadId: string) =>
  saves.some(save => save.status === 'failed' && save.threadId === threadId)

function setStatus(id: number, status: SaveStatus) {
  setSaves(saves.map(save => (save.id === id ? { ...save, status } : save)))
}

// Sends one save at a time, the oldest first, past the threads a failure parks.
// A save's retries are the shared client's mutation retry policy.
async function sendNext() {
  if (sending) return
  const save = saves.find(s => s.status === 'queued' && !parked(s.threadId))
  if (!save) return
  sending = true
  setStatus(save.id, 'running')
  try {
    await new MutationObserver(queryClient, { mutationFn: save.send }).mutate()
    setSaves(saves.filter(s => s.id !== save.id))
    save.onSuccess?.()
  } catch (error) {
    setStatus(save.id, 'failed')
    save.onError?.(error as Error)
  } finally {
    invalidateFor(save.event)
    sending = false
    void sendNext()
  }
}

function enqueue(save: Omit<SaveEntry, 'id' | 'status'>) {
  // The user acting again on a failed save's controls replaces the failed save
  // in its place, so a close queued behind it still waits for the replacement.
  const entry: SaveEntry = { ...save, id: nextId++, status: 'queued' }
  const failed = saves.some(s => s.status === 'failed' && s.item === save.item)
  setSaves(
    failed
      ? saves.map(s => (s.status === 'failed' && s.item === save.item ? entry : s))
      : [...saves, entry]
  )
  void sendNext()
}

/**
 * Whether the controls of a save item wait on a pending save (one in flight or
 * queued behind another).
 */
export function usePendingSaves(): (item: string) => boolean {
  const snapshot = useSaves()
  return item => snapshot.some(save => save.item === item && save.status !== 'failed')
}

/** Whether a save item's last save failed and was neither retried nor replaced. */
export function useFailedSave(): (item: string) => boolean {
  const snapshot = useSaves()
  return item => snapshot.some(save => save.item === item && save.status === 'failed')
}

export interface FailedSave {
  id: number
  threadId: string
  /** The controls the save came from. */
  item: string
  runId: string
  what: string
  /** Whether a close waits behind the failed save. */
  closeHeld: boolean
  retry: () => void
  discard: () => void
}

/** The saves that failed and still wait for the user, whatever page they came from. */
export function useFailedSaves(): FailedSave[] {
  const snapshot = useSaves()
  return snapshot
    .filter(save => save.status === 'failed')
    .map(save => ({
      id: save.id,
      threadId: save.threadId,
      item: save.item,
      runId: save.runId,
      what: save.what,
      closeHeld: snapshot.some(
        other => other.closes && other.status === 'queued' && other.threadId === save.threadId
      ),
      retry: () => {
        setStatus(save.id, 'queued')
        void sendNext()
      },
      discard: () => {
        setSaves(
          saves.filter(
            other =>
              other.id !== save.id &&
              !(other.closes && other.status === 'queued' && other.threadId === save.threadId)
          )
        )
        void sendNext()
      },
    }))
}

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

interface SaveCallbacks {
  onSuccess?: () => void
  onError?: (error: Error) => void
}

function useThreadSave<V extends SaveTarget>(
  itemOf: (vars: V) => string,
  send: (vars: V) => Promise<ThreadReviewResponse | undefined>,
  event: DomainEvent,
  what: string,
  closes = false
) {
  return {
    mutate: (vars: V, callbacks?: SaveCallbacks) =>
      enqueue({
        threadId: vars.threadId,
        runId: vars.runId,
        item: itemOf(vars),
        what,
        closes,
        event,
        send: async () => showRecorded(vars, await send(vars)),
        ...callbacks,
      }),
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
    'thread:labeled',
    'a flag label'
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
    'thread:labeled',
    'a clearing label'
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
    'thread:reply-reviewed',
    'a reply review'
  )
}

export interface ConfirmNonesVars extends SaveTarget {
  judgmentId: string
}

export function useConfirmNones() {
  return useThreadSave(
    (v: ConfirmNonesVars) => saveItem.nones(v.judgmentId),
    v => threadsApi.confirmNones(v.judgmentId, v.runId),
    'thread:nones-confirmed',
    'Confirm Nones'
  )
}

// Done and Next close a thread; the server decides what a close labels, after
// every save made before it.
export function useCloseThread() {
  return useThreadSave(
    (v: SaveTarget) => saveItem.close(v.threadId),
    v => threadsApi.close(v.threadId, v.runId),
    'thread:closed',
    'Done',
    true
  )
}
