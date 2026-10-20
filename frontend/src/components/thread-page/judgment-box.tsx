'use client'

import { useState, type ReactNode } from 'react'
import { clsx } from 'clsx'
import { Check, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { formatDateTime } from '@/components/timeline'
import { ReplyLists } from '@/components/threads/reply-lists'
import { FLAG_KINDS } from '@/components/threads/flag-kinds'
import { SAVE_ERROR, formatConfidence } from '@/components/threads/format'
import { StatusPill } from '@/components/threads/status-pill'
import {
  saveItem,
  useConfirmNones,
  useFailedSave,
  useLabelClearing,
  usePendingSaves,
} from '@/hooks/use-bench'
import type {
  ClosestUnflaggedResponse,
  NightClearingResponse,
  NightResponse,
  ThreadMessageResponse,
} from '@/types/generated/contact'
import { messageDomId } from './message'
import { count } from './night-label'

/** A link to a message of the thread: it scrolls to the message and highlights it. */
export function MessageLink({
  messageKey,
  onShow,
  children,
}: {
  messageKey: string
  onShow: (messageKey: string) => void
  children: ReactNode
}) {
  return (
    <a
      href={`#${messageDomId(messageKey)}`}
      onClick={event => {
        event.preventDefault()
        onShow(messageKey)
      }}
      className="text-blue-600 hover:text-blue-800 hover:underline"
    >
      {children}
    </a>
  )
}

function Section({ title, children }: { title?: string; children: ReactNode }) {
  return (
    <section className="space-y-2 border-t border-blue-100 px-4 py-3 first:border-t-0">
      {title && (
        <h4 className="text-xs font-medium uppercase tracking-wide text-gray-500">{title}</h4>
      )}
      {children}
    </section>
  )
}

function Clearing({
  threadId,
  runId,
  clearing,
  onShow,
}: {
  threadId: string
  runId: string
  clearing: NightClearingResponse
  onShow: (messageKey: string) => void
}) {
  const [changing, setChanging] = useState(false)
  const label = useLabelClearing()
  const isPending = usePendingSaves()
  const item = saveItem.clearing(clearing.commitment_message_key, clearing.cut_off)
  const pending = isPending(item)
  const failed = useFailedSave()(item)
  const reviewing = clearing.label === 'none' || changing
  const send = (done: boolean) =>
    label.mutate(
      {
        threadId,
        runId,
        commitmentMessageKey: clearing.commitment_message_key,
        cutOff: clearing.cut_off,
        done,
      },
      { onSuccess: () => setChanging(false) }
    )

  return (
    <li
      data-clearing
      data-clearing-label={clearing.label}
      className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border border-gray-200 bg-white px-3 py-2 text-sm"
    >
      <span data-clearing-text className="font-medium text-gray-900">
        Commitment from {formatDateTime(clearing.commitment_sent_at)} · judged{' '}
        {clearing.done ? 'done' : 'not done'}
      </span>
      <MessageLink messageKey={clearing.commitment_message_key} onShow={onShow}>
        <span className="text-xs">Show in thread ↑</span>
      </MessageLink>
      <span data-clearing-probability className="text-xs text-gray-600">
        {formatConfidence(clearing.probability)}
      </span>
      <span className="flex-1" />
      {clearing.label !== 'none' && !changing && (
        <span
          data-clearing-mark={clearing.label}
          className={clsx(
            'text-xs font-medium',
            clearing.label === 'confirmed' ? 'text-green-800' : 'text-gray-500'
          )}
        >
          {clearing.label === 'confirmed' ? 'Confirmed' : 'Removed'}
        </span>
      )}
      {reviewing && (
        <>
          <button
            type="button"
            aria-label="Confirm clearing"
            disabled={pending}
            onClick={() => send(clearing.done)}
            className="flex h-8 w-8 items-center justify-center rounded-md border border-gray-300 bg-white text-green-800 hover:bg-gray-50 disabled:opacity-50"
          >
            <Check aria-hidden="true" className="h-4 w-4" />
          </button>
          <button
            type="button"
            aria-label="Remove clearing"
            disabled={pending}
            onClick={() => send(!clearing.done)}
            className="flex h-8 w-8 items-center justify-center rounded-md border border-gray-300 bg-white text-gray-700 hover:bg-gray-50 disabled:opacity-50"
          >
            <X aria-hidden="true" className="h-4 w-4" />
          </button>
        </>
      )}
      {clearing.label !== 'none' && (
        <button
          type="button"
          aria-label={changing ? undefined : 'Change the clearing review'}
          disabled={pending}
          onClick={() => setChanging(!changing)}
          className="rounded-md px-1 text-sm font-medium text-blue-600 hover:bg-blue-50 hover:text-blue-800 disabled:opacity-50"
        >
          {changing ? 'Cancel' : 'Change'}
        </button>
      )}
      {failed && <span className="w-full text-xs text-red-700">{SAVE_ERROR}</span>}
    </li>
  )
}

function ClosestUnflagged({
  entries,
  floor,
  onShow,
}: {
  entries: ClosestUnflaggedResponse[]
  floor: number
  onShow: (messageKey: string) => void
}) {
  return (
    <p data-closest-unflagged className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-gray-500">
      <span>Closest unflagged:</span>
      {entries.map(({ kind, message, probability }) => (
        <span key={kind} data-closest-kind={kind}>
          {message && probability != null ? (
            <MessageLink messageKey={message.message_key} onShow={onShow}>
              {FLAG_KINDS[kind].name} {message.sender}, {formatDateTime(message.sent_at)} ↑{' '}
              {formatConfidence(probability)}
            </MessageLink>
          ) : (
            `${FLAG_KINDS[kind].name} none at ${formatConfidence(floor)} or more`
          )}
        </span>
      ))}
    </p>
  )
}

function ConfirmNones({
  threadId,
  runId,
  judgmentId,
}: {
  threadId: string
  runId: string
  judgmentId: string
}) {
  const confirm = useConfirmNones()
  const pending = usePendingSaves()(saveItem.nones(judgmentId))
  const failed = useFailedSave()(saveItem.nones(judgmentId))
  return (
    <>
      <Button
        type="button"
        size="sm"
        variant="outline"
        disabled={pending}
        onClick={() => confirm.mutate({ threadId, runId, judgmentId })}
      >
        <Check aria-hidden="true" className="mr-1.5 h-4 w-4 text-green-800" />
        Confirm Nones
      </Button>
      {failed && <span className="text-xs text-red-700">{SAVE_ERROR}</span>}
    </>
  )
}

interface JudgmentBoxProps {
  threadId: string
  runId: string
  night: NightResponse
  /** The thread's messages by key, for the first message the judgment read. */
  messages: Map<string, ThreadMessageResponse>
  /** The probability a message must reach to be named closest. */
  closestFloor: number
  onShow: (messageKey: string) => void
}

/**
 * A night's judgment: what it read, its reply lists, the clearing decisions it
 * made, the unflagged messages it came closest to flagging and, while the night
 * shows nothing and holds no label, Confirm Nones. The server decides every
 * state shown.
 */
export function JudgmentBox({
  threadId,
  runId,
  night,
  messages,
  closestFloor,
  onShow,
}: JudgmentBoxProps) {
  const first = messages.get(night.window_start)
  const firstTime = first ? formatDateTime(first.sent_at) : null
  return (
    <div
      data-judgment-box
      data-judgment-id={night.judgment_id}
      className="rounded-[10px] border border-blue-300 bg-[#f3f8ff] shadow-sm"
    >
      <Section>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h3 data-judgment-heading className="text-sm font-semibold text-gray-900">
            Judgment · read{' '}
            {first ? (
              <MessageLink messageKey={first.message_key} onShow={onShow}>
                {firstTime}
              </MessageLink>
            ) : null}{' '}
            to {formatDateTime(night.cut_off)}
          </h3>
          {night.to_review > 0 && <StatusPill status="to_review" toReview={night.to_review} />}
        </div>
      </Section>
      {night.proposal.reply_lists && (
        <Section>
          <ReplyLists proposal={night.proposal} runId={runId} />
        </Section>
      )}
      {night.clearings.length > 0 && (
        <Section title="Commitments re-checked">
          <ul className="space-y-2">
            {night.clearings.map(clearing => (
              <Clearing
                key={`${clearing.commitment_message_key}:${clearing.cut_off}`}
                threadId={threadId}
                runId={runId}
                clearing={clearing}
                onShow={onShow}
              />
            ))}
          </ul>
        </Section>
      )}
      <Section>
        <ClosestUnflagged entries={night.closest_unflagged} floor={closestFloor} onShow={onShow} />
      </Section>
      {(night.confirm_nones || night.labeled_none > 0) && (
        <Section>
          <div data-judgment-footer className="flex flex-wrap items-center gap-3">
            {night.confirm_nones && (
              <ConfirmNones threadId={threadId} runId={runId} judgmentId={night.judgment_id} />
            )}
            {night.labeled_none > 0 && (
              <span className="text-sm font-medium text-green-800">
                {`${count(night.labeled_none, 'unflagged new message')} labeled none when you moved on`}
              </span>
            )}
          </div>
        </Section>
      )}
    </div>
  )
}
