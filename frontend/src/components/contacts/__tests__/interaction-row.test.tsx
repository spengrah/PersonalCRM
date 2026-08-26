import { describe, expect, it } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import type {
  InteractionListItemResponse,
  InteractionContentKind,
  CallService,
  InteractionContentResponse,
} from '@/types/generated/contact'
import { formatDateTime, InteractionRow } from '../interaction-row'
import { InteractionContent } from '../interaction-content'

const baseItem = (
  overrides: Partial<InteractionListItemResponse> = {}
): InteractionListItemResponse => ({
  id: 'ixn-test',
  contact_id: 'contact-test',
  source: 'manual',
  occurred_at: '2026-01-02T03:04:00Z',
  direction: 'inbound',
  created_at: '2026-01-02T03:04:00Z',
  label: 'Synthetic interaction',
  content_kind: 'none',
  message_count: 0,
  is_group: false,
  venue_tags: [],
  ...overrides,
})

function renderItem(overrides: Partial<InteractionListItemResponse> = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <InteractionRow item={baseItem(overrides)} />
    </QueryClientProvider>
  )
}

describe('InteractionRow', () => {
  it.each([
    [45, '45s'],
    [372, '6m 12s'],
  ])('formats %s seconds as %s', (duration, expected) => {
    renderItem({
      source: 'phone_calls',
      content_kind: 'call',
      call: {
        service: 'voice',
        answered: true,
        has_voicemail: false,
        duration_seconds: duration,
      },
    })
    expect(screen.getByText(expected)).toBeVisible()
  })

  it.each([
    ['facetime_video', 'FaceTime video'],
    ['facetime_audio', 'FaceTime audio'],
  ] as [CallService, string][])('renders the %s service label', (service, expected) => {
    renderItem({
      source: 'phone_calls',
      content_kind: 'call',
      call: { service, answered: true, has_voicemail: false, duration_seconds: 60 },
    })
    expect(screen.getByText(expected)).toBeVisible()
  })

  it('renders missed and voicemail call states without a content indicator or expand affordance', () => {
    renderItem({
      source: 'phone_calls',
      content_kind: 'call',
      call: {
        service: 'voice',
        answered: false,
        has_voicemail: true,
        duration_seconds: 0,
      },
    })
    expect(screen.getByText('Missed')).toBeVisible()
    expect(screen.getByText('Voicemail')).toBeVisible()
    expect(screen.queryByText('No content')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /expand/i })).not.toBeInTheDocument()
  })

  it.each([
    ['messages', 1, '1 message'],
    ['messages', 3, '3 messages'],
    ['meeting_note', 0, 'Meeting note'],
    ['none', 0, 'No content'],
  ] as [InteractionContentKind, number, string][])(
    'renders %s as %s',
    (content_kind, message_count, expected) => {
      renderItem({ content_kind, message_count })
      expect(screen.getByText(expected)).toBeVisible()
    }
  )

  it('renders venue and group chips and preserves row identity attributes', () => {
    renderItem({
      id: 'ixn-with-tags',
      source: 'gchat',
      is_group: true,
      venue_tags: [
        { key: 'venue-key', label: 'Synthetic venue', kind: 'group_chat', is_group: true },
      ],
    })
    const row = screen.getByRole('listitem')
    expect(row).toHaveAttribute('data-interaction-id', 'ixn-with-tags')
    expect(row).toHaveAttribute('data-source', 'gchat')
    expect(screen.getByText('Google Chat')).toHaveAttribute('data-badge')
    expect(screen.getByText('Synthetic venue')).toHaveAttribute('data-venue-key', 'venue-key')
    expect(screen.getByText('Group')).toBeVisible()
  })

  it('does not render a Group chip when the interaction is not a group', () => {
    renderItem({ is_group: false })
    expect(screen.queryByText('Group', { exact: true })).not.toBeInTheDocument()
  })

  it.each(['inbound', 'outbound', 'mutual'] as const)(
    'labels the %s direction glyph',
    direction => {
      renderItem({ direction })
      expect(screen.getByLabelText(direction)).toBeVisible()
    }
  )

  it('omits the attendee line for one attendee and links event titles externally when linked', () => {
    renderItem({
      source: 'gcal',
      content_kind: 'none',
      event: {
        title: 'Synthetic event',
        start_time: '2026-01-02T03:04:00Z',
        end_time: '2026-01-02T04:04:00Z',
        attendee_count: 1,
        html_link: 'https://example.test/event',
      },
    })
    expect(screen.getByRole('link', { name: 'Synthetic event' })).toHaveAttribute(
      'target',
      '_blank'
    )
    expect(screen.getByRole('link', { name: 'Synthetic event' })).toHaveAttribute(
      'rel',
      'noopener noreferrer'
    )
    expect(screen.queryByText(/attendees/)).not.toBeInTheDocument()
  })

  it('renders two attendees and plain event titles without a link', () => {
    renderItem({
      source: 'gcal',
      event: {
        title: 'Synthetic plain event',
        start_time: '2026-01-02T03:04:00Z',
        end_time: '2026-01-02T04:04:00Z',
        attendee_count: 2,
      },
    })
    const row = screen.getByRole('listitem')
    expect(within(row).getByText('2 attendees')).toBeVisible()
    expect(
      within(row).queryByRole('link', { name: 'Synthetic plain event' })
    ).not.toBeInTheDocument()
    expect(within(row).getByText('Synthetic plain event')).toBeVisible()
  })

  it.each(['messages', 'meeting_note'] as const)(
    'renders an expand affordance for %s content',
    content_kind => {
      renderItem({ content_kind })
      const button = screen.getByRole('button', { name: 'Expand content' })
      expect(button).toHaveAttribute('aria-expanded', 'false')
    }
  )

  it('offers the expand affordance as an icon-only caret in the row action cluster', () => {
    renderItem({ content_kind: 'messages' })
    const button = screen.getByRole('button', { name: 'Expand content' })
    // Named by aria-label only: a visible text label would put a button in the
    // metadata chip row, which is the affordance the design canvas rejected.
    expect(button.textContent).toBe('')
    const actions = button.closest('[data-row-actions]')
    expect(actions).not.toBeNull()
    expect(
      within(actions as HTMLElement).getByRole('button', { name: 'More actions' })
    ).toBeVisible()
  })

  it('toggles the content region in place and unmounts it on collapse', () => {
    renderItem({ content_kind: 'messages' })
    const button = screen.getByRole('button', { name: 'Expand content' })
    fireEvent.click(button)
    expect(screen.getByRole('button', { name: 'Collapse content' })).toHaveAttribute(
      'aria-expanded',
      'true'
    )
    expect(document.querySelector('[data-content-region]')).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Collapse content' }))
    expect(document.querySelector('[data-content-region]')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Expand content' })).toHaveAttribute(
      'aria-expanded',
      'false'
    )
  })

  it.each(['call', 'none'] as const)(
    'does not render an expand affordance for %s content',
    content_kind => {
      renderItem({ content_kind })
      expect(screen.queryByRole('button', { name: /expand/i })).not.toBeInTheDocument()
    }
  )
})

const message = (
  id: string,
  sender: string,
  sent_at: string,
  body: string,
  venue_key = 'venue-a'
) => ({ id, sender, is_outgoing: false, sent_at, body, venue_key })

const messagesContent = (
  messages: InteractionContentResponse['messages']
): InteractionContentResponse => ({
  interaction_id: 'ixn-content',
  kind: 'messages',
  messages,
  meeting_notes: [],
})

describe('InteractionContent', () => {
  it('renders messages in API order with exact child text, including empty bodies', () => {
    const content = messagesContent([
      message('message-3', 'Sender C', '2026-01-02T03:06:00Z', ''),
      message('message-1', 'Sender A', '2026-01-02T03:04:00Z', 'First body'),
      message('message-2', 'Sender B', '2026-01-02T03:05:00Z', 'Second body'),
    ])
    render(<InteractionContent isPending={false} isError={false} content={content} />)
    const nodes = Array.from(document.querySelectorAll('[data-message-id]'))
    expect(nodes.map(node => node.getAttribute('data-message-id'))).toEqual([
      'message-3',
      'message-1',
      'message-2',
    ])
    content.messages.forEach((expectedMessage, index) => {
      const node = nodes[index]
      expect(node.querySelector('[data-message-sender]')?.textContent).toBe(expectedMessage.sender)
      expect(node.querySelector('[data-message-timestamp]')?.textContent).toBe(
        formatDateTime(expectedMessage.sent_at)
      )
      expect(node.querySelector('[data-message-body]')?.textContent).toBe(expectedMessage.body)
    })
    expect(nodes[0].querySelector('[data-message-body]')?.textContent).toBe('')
  })

  it('renders HTML-looking message bodies as literal text without elements', () => {
    const content = messagesContent([
      message('message-html', 'Sender', '2026-01-02T03:04:00Z', '<b>not bold</b>'),
    ])
    render(<InteractionContent isPending={false} isError={false} content={content} />)
    const region = document.querySelector('[data-content-region]') as HTMLElement
    expect(region.querySelector('[data-message-body]')).toHaveTextContent('<b>not bold</b>')
    expect(region.querySelector('b')).not.toBeInTheDocument()
  })

  it('filters messages by venue and renders all messages when the filter is absent', () => {
    const content = messagesContent([
      message('message-a', 'Sender A', '2026-01-02T03:04:00Z', 'A', 'venue-a'),
      message('message-b', 'Sender B', '2026-01-02T03:05:00Z', 'B', 'venue-b'),
    ])
    const { rerender } = render(
      <InteractionContent
        isPending={false}
        isError={false}
        content={content}
        venueFilter="venue-b"
      />
    )
    expect(document.querySelectorAll('[data-message-id]')).toHaveLength(1)
    expect(document.querySelector('[data-message-id]')).toHaveAttribute(
      'data-message-id',
      'message-b'
    )
    rerender(<InteractionContent isPending={false} isError={false} content={content} />)
    expect(document.querySelectorAll('[data-message-id]')).toHaveLength(2)
  })

  it('discloses the evidence scope only when it differs from the recorded count', () => {
    const content = messagesContent([
      message('message-a', 'Sender A', '2026-01-02T03:04:00Z', 'A', 'venue-a'),
      message('message-b', 'Sender B', '2026-01-02T03:05:00Z', 'B', 'venue-b'),
      message('message-c', 'Sender C', '2026-01-02T03:06:00Z', 'C', 'venue-b'),
    ])
    const scope = () => document.querySelector('[data-evidence-scope]')

    // Window wider than the interaction: the common case behind "header says 6,
    // thread shows 10".
    const { rerender } = render(
      <InteractionContent isPending={false} isError={false} content={content} recordedCount={2} />
    )
    expect(scope()).toHaveTextContent('Showing 3 messages from this conversation')
    expect(scope()).toHaveTextContent('2 recorded for this interaction')

    // A venue filter narrows BELOW the recorded count — also a mismatch, and
    // also disclosed, so the row's count never over-promises either way.
    rerender(
      <InteractionContent
        isPending={false}
        isError={false}
        content={content}
        venueFilter="venue-a"
        recordedCount={2}
      />
    )
    expect(scope()).toHaveTextContent('Showing 1 message from this conversation')

    // Agreement is silent: no note when the counts already match.
    rerender(
      <InteractionContent isPending={false} isError={false} content={content} recordedCount={3} />
    )
    expect(scope()).not.toBeInTheDocument()

    // No recorded count supplied (a caller that cannot know it) stays silent
    // rather than inventing a comparison.
    rerender(<InteractionContent isPending={false} isError={false} content={content} />)
    expect(scope()).not.toBeInTheDocument()
  })

  it('renders notes in API order, preserves null fields, and shows provenance once', () => {
    const content: InteractionContentResponse = {
      interaction_id: 'ixn-notes',
      kind: 'meeting_note',
      messages: [],
      meeting_notes: [
        { title: undefined, summary: 'Summary A', memo: '<i>memo A</i>' },
        { title: 'Title B', summary: 'Summary B', memo: 'Memo B' },
      ],
    }
    render(<InteractionContent isPending={false} isError={false} content={content} />)
    const notes = Array.from(document.querySelectorAll('[data-meeting-note]'))
    expect(notes).toHaveLength(2)
    expect(notes[0].querySelector('[data-note-title]')).not.toBeInTheDocument()
    const expectedNotes = [
      { summary: 'Summary A', memo: '<i>memo A</i>' },
      { title: 'Title B', summary: 'Summary B', memo: 'Memo B' },
    ]
    expectedNotes.forEach((expectedNote, index) => {
      const node = notes[index]
      if (expectedNote.title !== undefined) {
        expect(node.querySelector('[data-note-title]')?.textContent).toBe(expectedNote.title)
      }
      expect(node.querySelector('[data-note-summary]')?.textContent).toBe(expectedNote.summary)
      expect(node.querySelector('[data-note-memo]')?.textContent).toBe(expectedNote.memo)
    })
    expect(screen.getByText('Meeting notes are processed on-device.')).toBeInTheDocument()
    expect(screen.getAllByText('Meeting notes are processed on-device.')).toHaveLength(1)
    expect(document.querySelector('[data-content-region] i')).not.toBeInTheDocument()
  })

  it('renders loading and error states with exact copy', () => {
    const { rerender } = render(
      <InteractionContent isPending={true} isError={false} content={undefined} />
    )
    expect(document.querySelector('[data-content-region]')).toHaveTextContent('Loading content…')
    rerender(<InteractionContent isPending={false} isError={true} content={undefined} />)
    expect(document.querySelector('[data-content-region]')).toHaveTextContent(
      'Failed to load content.'
    )
  })
})
