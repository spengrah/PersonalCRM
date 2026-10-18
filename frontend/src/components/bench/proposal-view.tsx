'use client'

import { useState } from 'react'
import { clsx } from 'clsx'
import { ArrowUp, Check, Flag, Plus, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { formatDateTime } from '@/components/timeline'
import { useReviewOutcome } from '@/hooks/use-bench'
import type {
  BenchActResponse,
  BenchAdditionResponse,
  BenchItemResponse,
  BenchProposalResponse,
  ReviewJudgmentOutcomeRequest,
} from '@/types/generated/contact'
import { formatConfidence, formatDueDate } from './format'

type ProposalList = BenchItemResponse['list']

interface ProposalViewProps {
  proposal: BenchProposalResponse
  /** A proposal of another method on the same content starts collapsed. */
  collapsed: boolean
  onShowInThread: (contentRef: string) => void
}

const SAVE_ERROR = "Couldn't save. Try again."

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

/** The words naming an item or a picker choice. */
function partyText(
  list: ProposalList,
  name: string | undefined,
  debtor: string | undefined
): string {
  if (list === 'big_moment') return 'Big moment'
  if (list === 'commitments') return debtor === 'me' ? `Me → ${name}` : `${name} → Me`
  return name ?? ''
}

function itemText(item: BenchItemResponse): string {
  return partyText(item.list, item.contact?.name, item.debtor)
}

/** The review an act or a picker choice sends; the client never sends a verdict. */
function reviewOf(act: BenchActResponse): ReviewJudgmentOutcomeRequest {
  return act.keep_current_date
    ? { right_outcome: act.right_outcome, keep_current_date: true }
    : { right_outcome: act.right_outcome }
}

function Mark({ mark }: { mark: BenchItemResponse['mark'] }) {
  if (mark === 'confirmed') {
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
  if (mark === 'added') {
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
  if (mark === 'removed') {
    return (
      <span data-review-mark="removed" className="text-xs text-gray-500">
        Removed by you
      </span>
    )
  }
  return null
}

function ItemView({
  item,
  pending,
  failed,
  onReview,
  onShowInThread,
}: {
  item: BenchItemResponse
  pending: boolean
  failed: boolean
  onReview: (act: BenchActResponse) => void
  onShowInThread: (contentRef: string) => void
}) {
  const text = itemText(item)
  const label = item.list === 'big_moment' ? 'big moment' : text
  const look = item.state ? STATE_LOOK[item.state] : null
  // A reviewed item's acts are re-review controls, which this view does not offer.
  const acts = item.mark ? {} : Object.fromEntries(item.acts.map(act => [act.act, act]))
  const removed = item.mark === 'removed'
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
            className={clsx(
              'font-medium',
              removed ? 'text-gray-500 line-through' : 'text-gray-900'
            )}
          >
            {item.list === 'big_moment' ? (
              <span className="inline-flex items-center gap-1 rounded border border-pink-300 bg-pink-50 px-2 py-0.5 text-pink-800">
                <Flag aria-hidden="true" className="h-3.5 w-3.5" />
                {text}
              </span>
            ) : (
              text
            )}
            {item.list === 'commitments' && (
              <span data-due className="font-normal text-gray-600">
                {' · '}
                {item.due_date ? `due ${formatDueDate(item.due_date)}` : 'no stated date'}
              </span>
            )}
          </div>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
            {item.confidence != null && (
              <span data-confidence className="text-gray-600">
                {formatConfidence(item.confidence)}
              </span>
            )}
            {look && !removed && (
              <span data-state-label className={clsx('font-medium', look.text)}>
                {look.label}
              </span>
            )}
            <Mark mark={item.mark} />
          </div>
        </div>
        {(acts.confirm || acts.remove) && (
          <div className="flex flex-shrink-0 items-center gap-1">
            {acts.confirm && (
              <button
                type="button"
                aria-label={`Confirm ${label}`}
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
                aria-label={`Remove ${label}`}
                disabled={pending}
                onClick={() => onReview(acts.remove)}
                className="flex h-8 w-8 items-center justify-center rounded-md border border-gray-300 bg-white text-gray-600 hover:bg-gray-50 disabled:opacity-50"
              >
                <X aria-hidden="true" className="h-4 w-4" />
              </button>
            )}
          </div>
        )}
      </div>
      {item.change && (
        <div
          data-change={item.change.kind}
          className="mt-2 flex flex-wrap items-center gap-2 rounded-md border border-dashed border-amber-700 bg-amber-50 px-3 py-2 text-xs text-amber-800"
        >
          <span className="font-medium">
            {item.change.kind === 'remove'
              ? 'Suggested: remove'
              : `Suggested: due ${item.change.due_date ? formatDueDate(item.change.due_date) : 'later'}`}
          </span>
          {item.change.confidence != null && (
            <span>{formatConfidence(item.change.confidence)}</span>
          )}
          <span className="flex-1" />
          {acts.accept && (
            <Button
              type="button"
              size="sm"
              disabled={pending}
              onClick={() => onReview(acts.accept)}
            >
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
        </div>
      )}
      {item.source && !removed && (
        <blockquote
          data-source-message={item.source.content_ref}
          className="mt-2 border-l-2 border-gray-300 pl-3 text-xs text-gray-600"
        >
          <div>
            <span className="font-medium text-gray-700">{item.source.sender}</span> ·{' '}
            {formatDateTime(item.source.sent_at)}
          </div>
          <div className="text-gray-700">&ldquo;{item.source.text}&rdquo;</div>
          <button
            type="button"
            onClick={() => onShowInThread(item.source!.content_ref)}
            className="mt-1 inline-flex items-center gap-1 text-blue-600 hover:text-blue-800 hover:underline"
          >
            Show in thread
            <ArrowUp aria-hidden="true" className="h-3 w-3" />
          </button>
        </blockquote>
      )}
      {failed && <p className="mt-2 text-xs text-red-700">{SAVE_ERROR}</p>}
    </li>
  )
}

function ListSection({
  title,
  list,
  proposal,
  picker,
  setPicker,
  pending,
  failed,
  onReview,
  onShowInThread,
}: {
  title: string
  list: ProposalList
  proposal: BenchProposalResponse
  picker: ProposalList | null
  setPicker: (list: ProposalList | null) => void
  pending: boolean
  failed: string | null
  onReview: (outcomeId: string, body: ReviewJudgmentOutcomeRequest) => void
  onShowInThread: (contentRef: string) => void
}) {
  const items = proposal.items.filter(item => item.list === list)
  const additions = proposal.additions.filter(addition => addition.list === list)
  const add = (addition: BenchAdditionResponse) =>
    onReview(addition.outcome_id, { right_outcome: addition.right_outcome })
  const addFailed = additions.some(addition => addition.outcome_id === failed)
  return (
    <div role="group" aria-label={title} data-list={list} className="min-w-0 space-y-2">
      <h5 className="text-xs font-medium uppercase tracking-wide text-gray-500">{title}</h5>
      {items.length === 0 ? (
        <p className="text-sm text-gray-500">None</p>
      ) : (
        <ul className="space-y-2">
          {items.map(item => (
            <ItemView
              key={item.outcome_id}
              item={item}
              pending={pending}
              failed={failed === item.outcome_id}
              onReview={act => onReview(item.outcome_id, reviewOf(act))}
              onShowInThread={onShowInThread}
            />
          ))}
        </ul>
      )}
      {additions.length > 0 && list === 'big_moment' && (
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending}
          onClick={() => add(additions[0])}
        >
          Add
        </Button>
      )}
      {additions.length > 0 && list !== 'big_moment' && picker !== list && (
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={pending}
          onClick={() => setPicker(list)}
        >
          {list === 'commitments' ? 'Add commitment' : 'Add'}
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
              disabled={pending}
              onClick={() => add(addition)}
              className="rounded-full border border-gray-300 bg-white px-3 py-1 text-sm text-gray-700 hover:bg-gray-50 disabled:opacity-50"
            >
              {partyText(addition.list, addition.contact?.name, addition.debtor)}
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

/**
 * One judgment's proposal: the answers the server compiled from its question
 * tree, never the questions themselves, each item with its state and review
 * in place and corrected inline.
 */
export function ProposalView({
  proposal,
  collapsed: startCollapsed,
  onShowInThread,
}: ProposalViewProps) {
  const [collapsed, setCollapsed] = useState(startCollapsed)
  const [picker, setPicker] = useState<ProposalList | null>(null)
  const [failed, setFailed] = useState<string | null>(null)
  const review = useReviewOutcome()
  const send = (outcomeId: string, body: ReviewJudgmentOutcomeRequest) => {
    setFailed(null)
    review.mutate(
      { outcomeId, body },
      { onSuccess: () => setPicker(null), onError: () => setFailed(outcomeId) }
    )
  }
  const method = `${proposal.method_key} ${proposal.method_version}`
  const sections = {
    proposal,
    picker,
    setPicker,
    pending: review.isPending,
    failed,
    onReview: send,
    onShowInThread,
  }

  if (collapsed) {
    return (
      <section
        aria-label={`Proposal ${method}`}
        data-proposal={proposal.judgment_id}
        data-collapsed
        className="flex flex-wrap items-center gap-2 border-t border-gray-200 pt-3 text-sm"
      >
        <span className="font-medium text-gray-900">Proposal</span>
        <span className="text-gray-600">{method}</span>
        <span className="text-gray-600">
          {proposal.production ? 'production method' : 'Not acting'} · {proposal.items.length} item
          {proposal.items.length === 1 ? '' : 's'}
        </span>
        <span className="flex-1" />
        <Button type="button" size="sm" variant="outline" onClick={() => setCollapsed(false)}>
          Show proposal
        </Button>
      </section>
    )
  }

  return (
    <section
      aria-label={`Proposal ${method}`}
      data-proposal={proposal.judgment_id}
      className="space-y-4 border-t border-gray-200 pt-3"
    >
      <header className="flex flex-wrap items-center gap-2 text-sm">
        <span className="font-medium text-gray-900">Proposal</span>
        <span className="text-gray-600">{method}</span>
        {proposal.production && <span className="text-blue-700">production method</span>}
        {proposal.reused_at && (
          <span className="text-gray-600">Reused at {formatDateTime(proposal.reused_at)}</span>
        )}
        <span className="flex-1" />
        {proposal.to_review > 0 ? (
          <span data-proposal-status="to-review" className="text-sm font-medium text-amber-800">
            {proposal.to_review} to review
          </span>
        ) : (
          <span data-proposal-status="reviewed" className="text-sm font-medium text-green-800">
            All reviewed
          </span>
        )}
      </header>
      {proposal.reply_lists && (
        <div className="grid gap-4 sm:grid-cols-2">
          <ListSection title="I owe a reply" list="i_owe_reply" {...sections} />
          <ListSection title="Owes me a reply" list="owes_me_reply" {...sections} />
        </div>
      )}
      <div className={clsx(proposal.reply_lists && 'border-t border-gray-200 pt-4')}>
        <ListSection title="Commitments" list="commitments" {...sections} />
      </div>
      <div className="border-t border-gray-200 pt-4">
        <ListSection title="Big moment" list="big_moment" {...sections} />
      </div>
    </section>
  )
}
