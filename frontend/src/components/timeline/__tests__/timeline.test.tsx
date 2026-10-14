import { describe, expect, it, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactElement } from 'react'
import type { TimelineContentResponse, TimelineMemberResponse } from '@/types/generated/contact'

// The host below has no router, no contact page and no contact query: the only
// seam faked is the API client, so the requested route is what the tests read.
const mocks = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/lib/api-client', () => ({ apiClient: { get: mocks.get } }))

import {
  TimelineDrillDown,
  TimelineRow,
  type ParticipationContext,
  type TimelineIdentity,
  type TimelineItem,
  type TimelineRowAction,
} from '@/components/timeline'

const context: ParticipationContext = { contactId: 'contact-1' }

const member = (overrides: Partial<TimelineMemberResponse> = {}): TimelineMemberResponse => ({
  source: 'manual',
  label: 'Synthetic member',
  content_kind: 'none',
  message_count: 0,
  is_group: false,
  venue_tags: [],
  ...overrides,
})

const item = (
  kind: TimelineIdentity['kind'],
  members: TimelineMemberResponse[] = [member()],
  overrides: Partial<TimelineItem> = {}
): TimelineItem => ({
  identity: { kind, id: `${kind}-1` },
  direction: 'inbound',
  occurred_at: '2026-01-02T03:04:00Z',
  cadence_inert: kind === 'missed_call',
  members,
  ...overrides,
})

const message = (id: string, container: string | null, overrides = {}) => ({
  id,
  sender: `Sender ${id}`,
  is_outgoing: false,
  sent_at: '2026-01-02T03:04:00Z',
  body: `Body ${id}`,
  venue_key: container ?? '',
  source: 'gchat',
  channel: container ? ('group_chat' as const) : null,
  container,
  content_ref: `ref-${id}`,
  ...overrides,
})

const content = (
  identity: TimelineIdentity,
  members: TimelineContentResponse['members']
): TimelineContentResponse => ({ identity, members })

function renderInHost(ui: ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

beforeEach(() => {
  mocks.get.mockReset()
})

describe('TimelineRow and TimelineDrillDown in a bare host', () => {
  it('render outside the contact page from an identity and a context', async () => {
    const row = item('interaction', [member({ source: 'gcal', label: 'Planning' })])
    mocks.get.mockResolvedValue(content(row.identity, []))
    renderInHost(
      <>
        <TimelineRow item={row} context={context} />
        <TimelineDrillDown identity={row.identity} context={context} />
      </>
    )
    expect(screen.getByRole('listitem')).toHaveTextContent('Calendar')
    expect(screen.getByRole('listitem')).toHaveTextContent('Planning')
    await waitFor(() => expect(mocks.get).toHaveBeenCalled())
  })
})

describe('TimelineRow header', () => {
  it('shows the direction, the time and one source badge for every member', () => {
    renderInHost(
      <TimelineRow
        item={item('interaction', [member({ source: 'gcal' }), member({ source: 'phone_calls' })], {
          direction: 'mutual',
        })}
        context={context}
      />
    )
    expect(screen.getByLabelText('mutual')).toBeVisible()
    expect(document.querySelector('time')).toHaveAttribute('datetime', '2026-01-02T03:04:00Z')
    const badges = Array.from(document.querySelectorAll('[data-badge]')).map(b => b.textContent)
    expect(badges).toEqual(['Calendar', 'Call'])
  })
})

describe('TimelineRow actions and selection', () => {
  const actions = (calls: unknown[][]): TimelineRowAction[] => [
    { key: 'first', label: 'First action', onSelect: (...args) => calls.push(['first', ...args]) },
    {
      key: 'second',
      label: 'Second action',
      onSelect: (...args) => calls.push(['second', ...args]),
    },
  ]

  it('lists the passed actions in order for an interaction and calls onSelect with identity and context', async () => {
    const user = userEvent.setup()
    const calls: unknown[][] = []
    const row = item('interaction')
    renderInHost(<TimelineRow item={row} context={context} actions={actions(calls)} />)
    await user.click(screen.getByRole('button', { name: 'More actions' }))
    const entries = screen.getAllByRole('menuitem').map(entry => entry.textContent)
    expect(entries).toEqual(['First action', 'Second action'])
    await user.click(screen.getByRole('menuitem', { name: 'Second action' }))
    expect(calls).toEqual([['second', row.identity, context]])
  })

  it('renders the selection control reflecting selected, and toggling calls onToggle with the identity', async () => {
    const user = userEvent.setup()
    const onToggle = vi.fn()
    const row = item('interaction')
    const { rerender } = renderInHost(
      <TimelineRow item={row} context={context} selection={{ selected: false, onToggle }} />
    )
    const control = screen.getByRole('checkbox')
    expect(control).not.toBeChecked()
    await user.click(control)
    expect(onToggle).toHaveBeenCalledWith(row.identity)
    rerender(
      <QueryClientProvider client={new QueryClient()}>
        <TimelineRow item={row} context={context} selection={{ selected: true, onToggle }} />
      </QueryClientProvider>
    )
    expect(screen.getByRole('checkbox')).toBeChecked()
  })

  it.each(['message_burst', 'missed_call'] as const)(
    'renders no overflow menu and no selection control for %s although the host passes both',
    kind => {
      renderInHost(
        <TimelineRow
          item={item(kind)}
          context={context}
          actions={actions([])}
          selection={{ selected: true, onToggle: vi.fn() }}
        />
      )
      expect(screen.queryByRole('button', { name: 'More actions' })).not.toBeInTheDocument()
      expect(screen.queryByRole('menuitem')).not.toBeInTheDocument()
      expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    }
  )

  it('shows the reserved, disabled overflow affordance for an interaction with no actions', () => {
    renderInHost(<TimelineRow item={item('interaction')} context={context} />)
    expect(screen.getByRole('button', { name: 'More actions' })).toBeDisabled()
  })
})

describe('TimelineRow expansion', () => {
  const expandable = item('interaction', [member({ content_kind: 'messages', message_count: 1 })])

  it('expands to the host-supplied drill-down for the row identity and context', async () => {
    const user = userEvent.setup()
    const renderDrillDown = vi.fn((identity: TimelineIdentity, ctx: ParticipationContext) => (
      <p>{`custom ${identity.kind}:${identity.id} for ${ctx.contactId}`}</p>
    ))
    renderInHost(
      <TimelineRow item={expandable} context={context} renderDrillDown={renderDrillDown} />
    )
    await user.click(screen.getByRole('button', { name: 'Expand content' }))
    expect(screen.getByText('custom interaction:interaction-1 for contact-1')).toBeVisible()
    expect(renderDrillDown).toHaveBeenCalledWith(expandable.identity, context)
    expect(mocks.get).not.toHaveBeenCalled()
  })

  it('expands to TimelineDrillDown for the identity and context when the host passes none', async () => {
    const user = userEvent.setup()
    mocks.get.mockResolvedValue(
      content(expandable.identity, [
        {
          source: 'gchat',
          kind: 'messages',
          messages: [message('a', 'venue-a')],
          meeting_notes: [],
        },
      ])
    )
    renderInHost(<TimelineRow item={expandable} context={context} />)
    await user.click(screen.getByRole('button', { name: 'Expand content' }))
    expect(await screen.findByText('Body a')).toBeVisible()
    expect(mocks.get).toHaveBeenCalledWith(
      '/api/v1/timeline-items/interaction/interaction-1/content',
      {
        contact_id: 'contact-1',
      }
    )
  })
})

describe('TimelineDrillDown', () => {
  const identity: TimelineIdentity = { kind: 'interaction', id: 'node-1' }

  it('requests the content of its identity with the context contact and renders each member in its own section', async () => {
    mocks.get.mockResolvedValue(
      content(identity, [
        {
          source: 'gcal',
          kind: 'meeting_note',
          messages: [],
          meeting_notes: [
            {
              title: 'Standup',
              summary: 'Short summary',
              memo: 'Memo',
              source: 'anarlog_sessions',
              channel: 'meeting',
              container: 'anarlog_sessions:session-1',
              content_ref: 'ref-note',
            },
          ],
        },
        { source: 'phone_calls', kind: 'call', messages: [], meeting_notes: [] },
      ])
    )
    renderInHost(<TimelineDrillDown identity={identity} context={context} />)
    expect(await screen.findByText('Short summary')).toBeVisible()
    expect(mocks.get).toHaveBeenCalledWith('/api/v1/timeline-items/interaction/node-1/content', {
      contact_id: 'contact-1',
    })
    const sections = Array.from(document.querySelectorAll('[data-member-section]'))
    expect(sections.map(section => section.getAttribute('data-member-section'))).toEqual([
      'gcal',
      'phone_calls',
    ])
    expect(within(sections[0] as HTMLElement).getByText('Short summary')).toBeVisible()
    expect(within(sections[0] as HTMLElement).getByText('Meeting')).toHaveAttribute(
      'data-channel',
      'meeting'
    )
    expect(within(sections[1] as HTMLElement).queryByText('Short summary')).not.toBeInTheDocument()
  })

  it('labels each message with its own channel', async () => {
    mocks.get.mockResolvedValue(
      content(identity, [
        {
          source: 'email',
          kind: 'messages',
          messages: [
            message('mail', 'email:thread-1', { source: 'email', channel: 'email_thread' }),
            message('chat', 'gchat:space-1'),
          ],
          meeting_notes: [],
        },
      ])
    )
    renderInHost(<TimelineDrillDown identity={identity} context={context} />)
    await screen.findByText('Body mail')
    const channelOf = (id: string) =>
      document
        .querySelector(`[data-message-id="${id}"] [data-channel]`)
        ?.getAttribute('data-channel')
    expect(channelOf('mail')).toBe('email_thread')
    expect(channelOf('chat')).toBe('group_chat')
  })

  it('renders an annotation beside the content item its content_ref keys and nothing for an unkeyed one', async () => {
    mocks.get.mockResolvedValue(
      content(identity, [
        {
          source: 'gchat',
          kind: 'messages',
          messages: [message('a', 'venue-a'), message('b', 'venue-a')],
          meeting_notes: [],
        },
      ])
    )
    renderInHost(
      <TimelineDrillDown
        identity={identity}
        context={context}
        annotations={{
          'ref-b': <span>annotation for b</span>,
          'ref-nowhere': <span>annotation for nothing</span>,
        }}
      />
    )
    await screen.findByText('Body a')
    expect(
      within(document.querySelector('[data-message-id="b"]') as HTMLElement).getByText(
        'annotation for b'
      )
    ).toBeVisible()
    expect(
      within(document.querySelector('[data-message-id="a"]') as HTMLElement).queryByText(
        /annotation/
      )
    ).not.toBeInTheDocument()
    expect(screen.queryByText('annotation for nothing')).not.toBeInTheDocument()
  })

  it('shows only messages of the venueFilter container, and every message without it', async () => {
    mocks.get.mockResolvedValue(
      content(identity, [
        {
          source: 'gchat',
          kind: 'messages',
          messages: [message('a', 'venue-a'), message('b', 'venue-b')],
          meeting_notes: [],
        },
      ])
    )
    const { rerender } = renderInHost(
      <TimelineDrillDown identity={identity} context={context} venueFilter="venue-b" />
    )
    await screen.findByText('Body b')
    expect(document.querySelectorAll('[data-message-id]')).toHaveLength(1)
    expect(screen.queryByText('Body a')).not.toBeInTheDocument()
    rerender(
      <QueryClientProvider client={new QueryClient()}>
        <TimelineDrillDown identity={identity} context={context} />
      </QueryClientProvider>
    )
    await waitFor(() => expect(document.querySelectorAll('[data-message-id]')).toHaveLength(2))
  })
})
