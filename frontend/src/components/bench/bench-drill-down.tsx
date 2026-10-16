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
}

/** Proposals judged up to one cut-off, and the content they read. */
type Frame = {
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
 * content they read, the cut-off marked across the thread; content after the
 * last cut-off follows, subdued and marked as after the judgment.
 */
export function BenchDrillDown({ row, context }: BenchDrillDownProps) {
  const content = useTimelineContent(row.item.identity, context.contactId)
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
    const frame = frames.find(f => f.cutOff === proposal.cut_off)
    if (frame) frame.proposals.push(proposal)
    else frames.push({ cutOff: proposal.cut_off, proposals: [proposal], messages: [] })
  }
  frames.sort((a, b) => new Date(a.cutOff).getTime() - new Date(b.cutOff).getTime())
  const messages = (content.data?.members ?? [])
    .flatMap(member => member.messages)
    .sort((a, b) => new Date(a.sent_at).getTime() - new Date(b.sent_at).getTime())
  const notes = (content.data?.members ?? []).flatMap(member => member.meeting_notes)
  const after: ContentMessageResponse[] = []
  for (const message of messages) {
    const sent = new Date(message.sent_at).getTime()
    const frame = frames.find(f => sent <= new Date(f.cutOff).getTime())
    if (frame) frame.messages.push(message)
    else after.push(message)
  }
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
      {content.isPending && <p>Loading content…</p>}
      {content.isError && <p>Failed to load content.</p>}
      {frames.map((frame, index) => (
        <div
          key={frame.cutOff}
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
