import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'

import { SubTabs } from '../SubTabs'
import { OverlapMeter } from '../OverlapMeter'
import { SessionLede } from '../SessionLede'
import { InteractionsEmptyState } from '../InteractionsEmptyState'
import { OrphanCard } from '../OrphanCard'
import { ConflictCard } from '../ConflictCard'
import { NameCandidateRow } from '../NameCandidateRow'
import type {
  NeedsAttentionItem,
  NeedsAttentionCandidate,
  NameCandidateGroup,
} from '@/types/import'

describe('SubTabs', () => {
  it('renders both tabs and marks the active one selected', () => {
    render(<SubTabs active="people" attentionCount={0} onChange={() => {}} />)
    const people = screen.getByRole('tab', { name: /People/ })
    const interactions = screen.getByRole('tab', { name: /Interactions/ })
    expect(people).toHaveAttribute('aria-selected', 'true')
    expect(interactions).toHaveAttribute('aria-selected', 'false')
  })

  it('shows the amber badge only when attentionCount > 0', () => {
    const { rerender } = render(<SubTabs active="people" attentionCount={0} onChange={() => {}} />)
    expect(screen.queryByLabelText(/needing attention/)).not.toBeInTheDocument()

    rerender(<SubTabs active="people" attentionCount={3} onChange={() => {}} />)
    const badge = screen.getByLabelText('3 needing attention')
    expect(badge).toHaveTextContent('3')
  })

  it('calls onChange with the clicked tab', () => {
    const onChange = vi.fn()
    render(<SubTabs active="people" attentionCount={0} onChange={onChange} />)
    fireEvent.click(screen.getByRole('tab', { name: /Interactions/ }))
    expect(onChange).toHaveBeenCalledWith('interactions')
  })
})

describe('OverlapMeter', () => {
  it('drives the "N shared" label from overlapCount, not the matched-pip count', () => {
    // Two attendees, none flagged matched, but overlapCount=2 (authoritative).
    render(
      <OverlapMeter
        attendees={[
          { name: 'A', matched: false },
          { name: 'B', matched: false },
        ]}
        overlapCount={2}
      />
    )
    expect(screen.getByText('2 shared')).toBeInTheDocument()
  })

  it('renders one pip per attendee', () => {
    const { container } = render(
      <OverlapMeter
        attendees={[
          { name: 'A', matched: true },
          { name: 'B', matched: false },
          { name: 'C', matched: true },
        ]}
        overlapCount={2}
      />
    )
    // Pips carry a title attribute with the attendee name.
    expect(container.querySelectorAll('[title]')).toHaveLength(3)
  })
})

describe('SessionLede', () => {
  it('renders the title and the Anarlog session badge', () => {
    render(
      <SessionLede
        title="Quarterly review"
        meetingAt="2026-05-01T15:00:00Z"
        summaryExcerpt="A quick sync."
      />
    )
    expect(screen.getByRole('heading', { name: 'Quarterly review' })).toBeInTheDocument()
    expect(screen.getByText('Anarlog session')).toBeInTheDocument()
    expect(screen.getByText('A quick sync.')).toBeInTheDocument()
  })

  it('falls back to a placeholder when the title is null', () => {
    render(<SessionLede title={null} meetingAt="2026-05-01T15:00:00Z" />)
    expect(screen.getByRole('heading', { name: 'Untitled session' })).toBeInTheDocument()
  })
})

describe('InteractionsEmptyState', () => {
  it('renders the nothing-needs-attention message', () => {
    render(<InteractionsEmptyState />)
    expect(screen.getByText('Nothing needs attention')).toBeInTheDocument()
  })
})

const orphanItem: NeedsAttentionItem = {
  id: 'mn-1',
  anarlog_session_id: 'sess-1',
  mac_host_id: null,
  title: 'Hallway chat',
  summary_excerpt: null,
  meeting_at: '2026-05-01T15:00:00Z',
  linkage_state: 'orphan_needs_review',
  candidates: [],
}

describe('OrphanCard', () => {
  it('renders the Open Anarlog deep link as a bare hyprnote:// href', () => {
    render(<OrphanCard item={orphanItem} onLogImpromptu={() => {}} />)
    const link = screen.getByRole('link', { name: /Open Anarlog/ })
    expect(link).toHaveAttribute('href', 'hyprnote://')
  })

  it('calls onLogImpromptu when "Log as impromptu" is clicked', () => {
    const onLog = vi.fn()
    render(<OrphanCard item={orphanItem} onLogImpromptu={onLog} />)
    fireEvent.click(screen.getByRole('button', { name: /Log as impromptu/ }))
    expect(onLog).toHaveBeenCalledWith(orphanItem)
  })
})

const eventCandidate: NeedsAttentionCandidate = {
  kind: 'event',
  id: 'evt-1',
  occurred_at: '2026-05-01T15:05:00Z',
  overlap_count: 1,
  target_missing: false,
  preview: {
    title: 'Project sync',
    attendees: [
      { name: 'Matched Person', matched: true },
      { name: 'Other Person', matched: false },
    ],
  },
}

const conflictItem: NeedsAttentionItem = {
  id: 'mn-2',
  anarlog_session_id: 'sess-2',
  mac_host_id: null,
  title: 'Ambiguous meeting',
  summary_excerpt: 'Could be either.',
  meeting_at: '2026-05-01T15:00:00Z',
  linkage_state: 'conflict_pending',
  candidates: [eventCandidate],
}

describe('ConflictCard', () => {
  it('renders a candidate row with attendee names and the overlap meter', () => {
    render(<ConflictCard item={conflictItem} onPick={() => {}} onLogImpromptu={() => {}} />)
    expect(screen.getByText('Project sync')).toBeInTheDocument()
    // Attendee names render with a trailing comma separator for all but the
    // last, so match the leading text rather than the exact node content.
    expect(screen.getByText(/Matched Person/)).toBeInTheDocument()
    expect(screen.getByText(/Other Person/)).toBeInTheDocument()
    expect(screen.getByText('1 shared')).toBeInTheDocument()
  })

  it('calls onPick with the item and candidate when "This one" is clicked', () => {
    const onPick = vi.fn()
    render(<ConflictCard item={conflictItem} onPick={onPick} onLogImpromptu={() => {}} />)
    fireEvent.click(screen.getByRole('button', { name: /This one/ }))
    expect(onPick).toHaveBeenCalledWith(conflictItem, eventCandidate)
  })

  it('disables "This one" for a target-missing candidate', () => {
    const missing: NeedsAttentionItem = {
      ...conflictItem,
      candidates: [{ ...eventCandidate, target_missing: true }],
    }
    render(<ConflictCard item={missing} onPick={() => {}} onLogImpromptu={() => {}} />)
    expect(screen.getByRole('button', { name: /This one/ })).toBeDisabled()
    expect(screen.getByText(/no longer exists/)).toBeInTheDocument()
  })
})

const group: NameCandidateGroup = {
  normalized_token: 'lena',
  token_display: 'Lena',
  evidence_count: 2,
  session_titles: ['1:1 with Lena', 'Lena / sync'],
}

describe('NameCandidateRow', () => {
  it('renders the display token and the low-confidence chip', () => {
    render(<NameCandidateRow group={group} onCreate={() => {}} onIgnore={() => {}} />)
    expect(screen.getByText('Lena')).toBeInTheDocument()
    expect(screen.getByText(/low confidence/)).toBeInTheDocument()
  })

  it('expands to show the session-title evidence', () => {
    render(<NameCandidateRow group={group} onCreate={() => {}} onIgnore={() => {}} />)
    // Evidence is collapsed until the toggle is clicked.
    expect(screen.queryByText('1:1 with Lena')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /Seen in 2 session titles/ }))
    expect(screen.getByText('1:1 with Lena')).toBeInTheDocument()
    expect(screen.getByText('Lena / sync')).toBeInTheDocument()
  })

  it('wires the create and ignore callbacks', () => {
    const onCreate = vi.fn()
    const onIgnore = vi.fn()
    render(<NameCandidateRow group={group} onCreate={onCreate} onIgnore={onIgnore} />)
    fireEvent.click(screen.getByRole('button', { name: /Create contact/ }))
    expect(onCreate).toHaveBeenCalledWith(group)
    fireEvent.click(screen.getByRole('button', { name: /Not a person/ }))
    expect(onIgnore).toHaveBeenCalledWith(group)
  })
})
