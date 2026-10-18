'use client'

import { useRef, useState } from 'react'
import { ArrowUp } from 'lucide-react'
import {
  TimelineDrillDown,
  TimelineMessage,
  TimelineNote,
  formatDateTime,
  type ParticipationContext,
} from '@/components/timeline'
import { useThreadContents } from '@/hooks/use-bench'
import { useTimelineContent } from '@/hooks/use-interactions'
import type {
  AllInteractionItemResponse,
  BenchProposalResponse,
  ContentMessageResponse,
} from '@/types/generated/contact'
import { ProposalView } from './proposal-view'

interface BenchDrillDownProps {
  row: AllInteractionItemResponse
  context: ParticipationContext
  /** The run the view is narrowed to, which a save on a proposal carries. */
  runId?: string
  /** Whether the row's last close is still being saved, which its proposals wait on. */
  closing: boolean
}

/** Proposals of one thread judged up to one cut-off, and the content they read. */
type Frame = {
  thread: string
  cutOff: string
  proposals: BenchProposalResponse[]
  messages: ContentMessageResponse[]
}

function CutOffRule({ cutOff }: { cutOff: string }) {
  return (
    <div
      data-cut-off={cutOff}
      className="flex items-center gap-2 text-xs font-medium text-blue-700"
    >
      <span className="h-px flex-1 bg-blue-300" />
      <ArrowUp aria-hidden="true" className="h-3.5 w-3.5" />
      <span>judged up to {formatDateTime(cutOff)}</span>
      <span className="h-px flex-1 bg-blue-300" />
    </div>
  )
}

/**
 * A row's drill-down. With no judgment it is the contact history's. With
 * judgments, each cut-off's proposals sit in a frame directly beneath the
 * content of their thread they read, the whole thread up to the cut-off, the
 * cut-off marked across the thread; the thread's content after its last
 * cut-off follows, subdued and marked as after the judgment. A meeting's
 * content is its notes, which the row's content holds.
 */
export function BenchDrillDown({ row, context, runId, closing }: BenchDrillDownProps) {
  const content = useTimelineContent(row.item.identity, context.contactId)
  const threadIds = [...new Set(row.proposals.map(proposal => proposal.thread_id))]
  const threads = useThreadContents(threadIds)
  const region = useRef<HTMLDivElement>(null)
  const [highlighted, setHighlighted] = useState<string | null>(null)

  if (row.proposals.length === 0) {
    return <TimelineDrillDown identity={row.item.identity} context={context} />
  }

  const showInThread = (contentRef: string) => {
    setHighlighted(contentRef)
    region.current
      ?.querySelector(`[data-content-ref="${CSS.escape(contentRef)}"]`)
      ?.scrollIntoView({ block: 'center', behavior: 'smooth' })
  }

  const frames: Frame[] = []
  for (const proposal of row.proposals) {
    const frame = frames.find(f => f.thread === proposal.thread_id && f.cutOff === proposal.cut_off)
    if (frame) frame.proposals.push(proposal)
    else
      frames.push({
        thread: proposal.thread_id,
        cutOff: proposal.cut_off,
        proposals: [proposal],
        messages: [],
      })
  }
  frames.sort((a, b) => new Date(a.cutOff).getTime() - new Date(b.cutOff).getTime())
  const notes = (content.data?.members ?? []).flatMap(member => member.meeting_notes)
  const after: ContentMessageResponse[] = []
  threadIds.forEach((thread, i) => {
    for (const message of threads[i].data?.messages ?? []) {
      const sent = new Date(message.sent_at).getTime()
      const frame = frames.find(f => f.thread === thread && sent <= new Date(f.cutOff).getTime())
      if (frame) frame.messages.push(message)
      else after.push(message)
    }
  })
  after.sort((a, b) => new Date(a.sent_at).getTime() - new Date(b.sent_at).getTime())
  const pending = content.isPending || threads.some(thread => thread.isPending)
  const failed = content.isError || threads.some(thread => thread.isError)
  const shown = (message: ContentMessageResponse) => (
    <div
      key={message.id}
      data-highlighted={highlighted === message.content_ref || undefined}
      className={
        highlighted === message.content_ref
          ? 'rounded bg-amber-50 p-2 ring-2 ring-amber-300'
          : undefined
      }
    >
      <TimelineMessage message={message} />
    </div>
  )

  return (
    <div
      ref={region}
      data-content-region
      className="mt-4 space-y-4 rounded bg-gray-50 p-4 text-sm text-gray-700"
    >
      {pending && <p>Loading content…</p>}
      {failed && <p>Failed to load content.</p>}
      {frames.map((frame, index) => (
        <div
          key={`${frame.thread} ${frame.cutOff}`}
          data-judged-frame
          className="space-y-4 rounded border border-blue-200 bg-white p-4"
        >
          <div data-judged-content className="space-y-4">
            {index === 0 && notes.map(note => <TimelineNote key={note.content_ref} note={note} />)}
            {frame.messages.map(shown)}
          </div>
          <CutOffRule cutOff={frame.cutOff} />
          {frame.proposals.map((proposal, i) => (
            <ProposalView
              key={proposal.judgment_id}
              proposal={proposal}
              runId={runId}
              closing={closing}
              collapsed={i > 0}
              onShowInThread={showInThread}
            />
          ))}
        </div>
      ))}
      {after.length > 0 && (
        <div data-after-judgment className="space-y-4 opacity-60">
          {after.map(message => (
            <div key={message.id} className="space-y-1">
              <span className="inline-flex rounded bg-gray-200 px-2 py-0.5 text-xs text-gray-700">
                After the judgment
              </span>
              {shown(message)}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
