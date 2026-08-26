import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'

import type {
  InteractionListItemResponse,
  InteractionContentKind,
  CallService,
} from '@/types/generated/contact'
import { InteractionRow } from '../interaction-row'

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
  return render(<InteractionRow item={baseItem(overrides)} />)
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
})
