'use client'

import { useState } from 'react'
import { clsx } from 'clsx'
import { Check, Plus, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { saveItem, useFailedSave, usePendingSaves, useReviewReplyItem } from '@/hooks/use-bench'
import type {
  BenchActResponse,
  BenchAdditionResponse,
  BenchItemResponse,
  BenchListResponse,
  BenchProposalResponse,
  ReviewJudgmentOutcomeRequest,
} from '@/types/generated/contact'
import type { ProposalList } from '@/types/generated/judgment'
import { SAVE_ERROR, formatConfidence } from './format'

type ReplyList = Extract<ProposalList, 'i_owe_reply' | 'owes_me_reply'>

const LIST_TITLES: Record<ReplyList, string> = {
  i_owe_reply: 'I owe a reply',
  owes_me_reply: 'Owes me a reply',
}

const isReplyList = (entry: BenchListResponse): entry is BenchListResponse & { list: ReplyList } =>
  entry.list in LIST_TITLES

const STATE_LOOK: Record<
  NonNullable<BenchItemResponse['state']>,
  { box: string; label: string; text: string }
> = {
  live: { box: 'border border-blue-200 bg-blue-50', label: 'Live', text: 'text-blue-700' },
  live_with_change: {
    box: 'border border-blue-200 bg-blue-50',
    label: 'Live',
    text: 'text-blue-700',
  },
  suggested: {
    box: 'border border-dashed border-amber-700 bg-amber-50',
    label: 'Suggested',
    text: 'text-amber-800',
  },
  not_acting: {
    box: 'border border-gray-300 bg-gray-50',
    label: 'Not acting',
    text: 'text-gray-600',
  },
}

/** The review an act or a picker choice sends; the client never sends a verdict. */
function reviewOf(act: BenchActResponse): ReviewJudgmentOutcomeRequest {
  return act.keep_current_date
    ? { right_outcome: act.right_outcome, keep_current_date: true }
    : { right_outcome: act.right_outcome }
}

function Mark({ item }: { item: BenchItemResponse }) {
  if (item.mark === 'confirmed') {
    return (
      <span
        data-review-mark="confirmed"
        className="inline-flex items-center gap-1 text-xs font-medium text-green-800"
      >
        <Check aria-hidden="true" className="h-3.5 w-3.5" />
        Confirmed
      </span>
    )
  }
  if (item.mark === 'added') {
    return (
      <span
        data-review-mark="added"
        className="inline-flex items-center gap-1 text-xs font-medium text-green-800"
      >
        <Plus aria-hidden="true" className="h-3.5 w-3.5" />
        Added by you
      </span>
    )
  }
  if (item.mark === 'removed') {
    return (
      <span data-review-mark="removed" className="text-xs text-gray-500">
        Removed by you
      </span>
    )
  }
  return null
}

/**
 * The mark of a list's Nones once every negative decision behind the list has
 * a label: "Confirmed" on a None line, and on a heading line, above the list's
 * items, "Nones confirmed", so it cannot be read as the mark of an item.
 */
function NonesMark({ heading = false }: { heading?: boolean }) {
  return (
    <span
      data-nones-mark
      className="inline-flex items-center gap-1 text-xs font-medium text-green-800"
    >
      <Check aria-hidden="true" className="h-3.5 w-3.5" />
      {heading ? 'Nones confirmed' : 'Confirmed'}
    </span>
  )
}

function ItemView({
  item,
  changing,
  pending,
  failed,
  onReview,
  onChange,
}: {
  item: BenchItemResponse
  /** Whether the user chose Change on this reviewed item. */
  changing: boolean
  pending: boolean
  failed: boolean
  onReview: (act: BenchActResponse) => void
  onChange: (changing: boolean) => void
}) {
  const name = item.contact?.name ?? ''
  const look = item.state ? STATE_LOOK[item.state] : null
  // A reviewed item's acts are its re-review controls, offered behind Change.
  const reviewing = !item.mark || changing
  const acts = reviewing ? Object.fromEntries(item.acts.map(act => [act.act, act])) : {}
  // The server sends no state only for a removal that took effect.
  const struck = !item.state
  const changeActs = (
    <>
      {acts.accept && (
        <Button type="button" size="sm" disabled={pending} onClick={() => onReview(acts.accept)}>
          Accept
        </Button>
      )}
      {acts.keep && (
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending}
          onClick={() => onReview(acts.keep)}
        >
          Keep
        </Button>
      )}
      {acts.keep_original && (
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending}
          onClick={() => onReview(acts.keep_original)}
        >
          Keep original
        </Button>
      )}
    </>
  )
  return (
    <li
      data-item
      data-outcome-id={item.outcome_id}
      data-state={item.state}
      data-mark={item.mark}
      className={clsx('rounded-md p-3', look ? look.box : 'border border-gray-200 bg-white')}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <div
            data-item-text
            className={clsx('font-medium', struck ? 'text-gray-500 line-through' : 'text-gray-900')}
          >
            {name}
          </div>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
            {item.confidence != null && (
              <span data-confidence className="text-gray-600">
                {formatConfidence(item.confidence)}
              </span>
            )}
            {look && (
              <span data-state-label className={clsx('font-medium', look.text)}>
                {look.label}
              </span>
            )}
            {!changing && <Mark item={item} />}
          </div>
        </div>
        <div className="flex flex-shrink-0 items-center gap-1">
          {acts.confirm && (
            <button
              type="button"
              aria-label={`Confirm ${name}`}
              disabled={pending}
              onClick={() => onReview(acts.confirm)}
              className="flex h-8 w-8 items-center justify-center rounded-md border border-gray-300 bg-white text-green-700 hover:bg-gray-50 disabled:opacity-50"
            >
              <Check aria-hidden="true" className="h-4 w-4" />
            </button>
          )}
          {acts.remove && (
            <button
              type="button"
              aria-label={`Remove ${name}`}
              disabled={pending}
              onClick={() => onReview(acts.remove)}
              className="flex h-8 w-8 items-center justify-center rounded-md border border-gray-300 bg-white text-gray-600 hover:bg-gray-50 disabled:opacity-50"
            >
              <X aria-hidden="true" className="h-4 w-4" />
            </button>
          )}
          {!item.change && changeActs}
          {item.mark && (
            <button
              type="button"
              aria-label={changing ? undefined : `Change the review of ${name}`}
              disabled={pending}
              onClick={() => onChange(!changing)}
              className="rounded-md px-2 py-1 text-sm font-medium text-blue-600 hover:bg-blue-50 hover:text-blue-800 disabled:opacity-50"
            >
              {changing ? 'Cancel' : 'Change'}
            </button>
          )}
        </div>
      </div>
      {item.change && (
        <div
          data-change={item.change.kind}
          className="mt-2 flex flex-wrap items-center gap-2 rounded-md border border-dashed border-amber-700 bg-amber-50 px-3 py-2 text-xs text-amber-800"
        >
          <span className="font-medium">Suggested: remove</span>
          {item.change.confidence != null && (
            <span>{formatConfidence(item.change.confidence)}</span>
          )}
          <span className="flex-1" />
          {changeActs}
        </div>
      )}
      {failed && <p className="mt-2 text-xs text-red-700">{SAVE_ERROR}</p>}
    </li>
  )
}

function ListSection({
  entry,
  proposal,
  picker,
  setPicker,
  changing,
  setChanging,
  pending,
  failed,
  onReview,
}: {
  entry: BenchListResponse & { list: ReplyList }
  proposal: BenchProposalResponse
  picker: ProposalList | null
  setPicker: (list: ProposalList | null) => void
  changing: string | null
  setChanging: (outcomeId: string | null) => void
  pending: (outcomeId: string) => boolean
  failed: (outcomeId: string) => boolean
  onReview: (outcomeId: string, body: ReviewJudgmentOutcomeRequest) => void
}) {
  const { list, closest } = entry
  const title = LIST_TITLES[list]
  const items = proposal.items.filter(item => item.list === list)
  const additions = proposal.additions.filter(addition => addition.list === list)
  const add = (addition: BenchAdditionResponse) =>
    onReview(addition.outcome_id, { right_outcome: addition.right_outcome })
  const addFailed = additions.some(addition => failed(addition.outcome_id))
  const addPending = additions.some(addition => pending(addition.outcome_id))
  const closestName = closest?.contact?.name ?? ''
  return (
    <div role="group" aria-label={title} data-list={list} className="min-w-0 space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <h5 className="text-xs font-medium uppercase tracking-wide text-gray-500">{title}</h5>
        {items.length > 0 && closest && (
          <span data-closest className="inline-flex items-center gap-1.5 text-xs text-gray-500">
            closest None: {closestName} · {closest.percent}%
            {entry.nones_reviewed && <NonesMark heading />}
          </span>
        )}
      </div>
      {items.length === 0 ? (
        <div
          data-none
          className={clsx(
            'flex flex-wrap items-baseline gap-x-2 gap-y-1 rounded-md bg-white px-3 py-2',
            entry.nones_reviewed ? 'border border-gray-200' : 'border border-dashed border-gray-300'
          )}
        >
          <span className="text-sm font-medium text-gray-600">None</span>{' '}
          {closest && (
            <>
              <span data-closest className="text-sm text-gray-500">
                closest: {closestName}
              </span>{' '}
              <span className="text-xs text-gray-600">{closest.percent}%</span>{' '}
            </>
          )}
          {entry.nones_reviewed && <NonesMark />}
        </div>
      ) : (
        <ul className="space-y-2">
          {items.map(item => (
            <ItemView
              key={item.outcome_id}
              item={item}
              changing={changing === item.outcome_id}
              pending={pending(item.outcome_id)}
              failed={failed(item.outcome_id)}
              onReview={act => onReview(item.outcome_id, reviewOf(act))}
              onChange={on => setChanging(on ? item.outcome_id : null)}
            />
          ))}
        </ul>
      )}
      {additions.length > 0 && picker !== list && (
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={addPending}
          onClick={() => setPicker(list)}
        >
          Add
        </Button>
      )}
      {additions.length > 0 && picker === list && (
        <div
          role="group"
          aria-label={`Add to ${title}`}
          className="flex flex-wrap items-center gap-2"
        >
          {additions.map(addition => (
            <button
              key={`${addition.outcome_id}-${addition.right_outcome}`}
              type="button"
              disabled={pending(addition.outcome_id)}
              onClick={() => add(addition)}
              className="rounded-full border border-gray-300 bg-white px-3 py-1 text-sm text-gray-700 hover:bg-gray-50 disabled:opacity-50"
            >
              {addition.contact?.name}
            </button>
          ))}
          <button
            type="button"
            aria-label="Close"
            onClick={() => setPicker(null)}
            className="flex h-8 w-8 items-center justify-center rounded-md text-gray-500 hover:bg-gray-100"
          >
            <X aria-hidden="true" className="h-4 w-4" />
          </button>
        </div>
      )}
      {addFailed && <p className="text-xs text-red-700">{SAVE_ERROR}</p>}
    </div>
  )
}

interface ReplyListsProps {
  proposal: BenchProposalResponse
  /** The run whose review state a save's answer carries. */
  runId: string
}

/**
 * A judgment's reply lists, "I owe a reply" and "Owes me a reply" side by side:
 * the answers the server compiled from its question tree, never the questions
 * themselves, each item with its state and review in place and corrected
 * inline, and each list's Nones with the decision the judgment came closest to
 * making. The server decides every status, control and state shown here. A
 * save disables only the controls of the item it came from.
 */
export function ReplyLists({ proposal, runId }: ReplyListsProps) {
  const [picker, setPicker] = useState<ProposalList | null>(null)
  const [changing, setChanging] = useState<string | null>(null)
  const review = useReviewReplyItem()
  const isPending = usePendingSaves()
  const isFailed = useFailedSave()
  const send = (outcomeId: string, body: ReviewJudgmentOutcomeRequest) =>
    review.mutate(
      { threadId: proposal.thread_id, runId, outcomeId, review: body },
      {
        onSuccess: () => {
          setPicker(null)
          setChanging(null)
        },
      }
    )
  const lists = proposal.lists.filter(isReplyList)
  if (lists.length === 0) return null

  return (
    <div className="grid gap-4 sm:grid-cols-2">
      {lists.map(entry => (
        <ListSection
          key={entry.list}
          entry={entry}
          proposal={proposal}
          picker={picker}
          setPicker={setPicker}
          changing={changing}
          setChanging={setChanging}
          pending={outcomeId => isPending(saveItem.reply(outcomeId))}
          failed={outcomeId => isFailed(saveItem.reply(outcomeId))}
          onReview={send}
        />
      ))}
    </div>
  )
}
