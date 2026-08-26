import { describe, expect, it, beforeAll, afterAll, vi } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import type { VenueTagResponse } from '@/types/generated/contact'
import {
  InteractionFilters,
  DEFAULT_FILTER_SELECTION,
  boundUpcomingEvents,
  customRangeToParams,
  selectionToFilters,
  upcomingVisible,
  type InteractionFilterSelection,
} from '../interaction-filters'

const originalTZ = process.env.TZ

beforeAll(() => {
  process.env.TZ = 'America/Chicago'
})

afterAll(() => {
  if (originalTZ === undefined) delete process.env.TZ
  else process.env.TZ = originalTZ
})

const selection = (
  overrides: Partial<InteractionFilterSelection> = {}
): InteractionFilterSelection => ({
  ...DEFAULT_FILTER_SELECTION,
  ...overrides,
})

describe('customRangeToParams', () => {
  it.each([
    ['2026-03-08', '2026-03-08', '2026-03-08T06:00:00.000Z', '2026-03-09T05:00:00.000Z'],
    ['2026-11-01', '2026-11-01', '2026-11-01T05:00:00.000Z', '2026-11-02T06:00:00.000Z'],
    ['2026-08-30', '2026-08-31', '2026-08-30T05:00:00.000Z', '2026-09-01T05:00:00.000Z'],
  ])('serializes %s through local midnight and an exclusive next day', (start, end, from, to) => {
    expect(customRangeToParams(start, end)).toEqual({ from, to })
  })

  it.each([
    ['2026-03-08', '2026-03-08'],
    ['2026-11-01', '2026-11-01'],
    ['2026-08-30', '2026-08-31'],
  ])('keeps the serialized instants at local date boundaries for %s..%s', (start, end) => {
    const params = customRangeToParams(start, end)
    const from = new Date(params.from)
    const to = new Date(params.to)
    const [sy, sm, sd] = start.split('-').map(Number)
    const [ey, em, ed] = end.split('-').map(Number)
    const endDate = new Date(ey, em - 1, ed)
    endDate.setDate(endDate.getDate() + 1)
    expect([
      from.getFullYear(),
      from.getMonth() + 1,
      from.getDate(),
      from.getHours(),
      from.getMinutes(),
    ]).toEqual([sy, sm, sd, 0, 0])
    expect([
      to.getFullYear(),
      to.getMonth() + 1,
      to.getDate(),
      to.getHours(),
      to.getMinutes(),
    ]).toEqual([endDate.getFullYear(), endDate.getMonth() + 1, endDate.getDate(), 0, 0])
  })
})

describe('selection helpers', () => {
  const now = new Date('2031-05-10T12:00:00.000Z')

  it.each([
    ['all', {}],
    ['30d', { from: new Date(now.getTime() - 30 * 86400000).toISOString() }],
    ['90d', { from: new Date(now.getTime() - 90 * 86400000).toISOString() }],
  ] as const)('maps the %s selection to its date params', (preset, expected) => {
    expect(selectionToFilters(selection({ preset }), now)).toEqual(expected)
  })

  it('combines a venue with date filters and leaves incomplete custom ranges unfiltered', () => {
    expect(selectionToFilters(selection({ preset: '30d', venue: 'venue-a' }), now)).toEqual({
      venue: 'venue-a',
      from: new Date(now.getTime() - 30 * 86400000).toISOString(),
    })
    expect(
      selectionToFilters(
        selection({ preset: 'custom', customStart: '2031-05-01', customEnd: '' }),
        now
      )
    ).toEqual({})
    expect(
      selectionToFilters(
        selection({ preset: 'custom', customStart: '2031-05-01', customEnd: '2031-05-03' }),
        now
      )
    ).toEqual(customRangeToParams('2031-05-01', '2031-05-03'))
  })

  it.each([
    [selection({}), now, true],
    [selection({ venue: 'venue-a' }), now, false],
    [selection({ preset: '30d' }), now, false],
    [selection({ preset: '90d' }), now, false],
    [
      selection({ preset: 'custom', customStart: '2031-05-01', customEnd: '2031-05-20' }),
      now,
      true,
    ],
    [
      selection({ preset: 'custom', customStart: '2031-04-01', customEnd: '2031-04-02' }),
      now,
      false,
    ],
    [
      selection({ preset: 'custom', customStart: '2031-05-10', customEnd: '2031-05-10' }),
      new Date(customRangeToParams('2031-05-10', '2031-05-10').to),
      false,
    ],
    [selection({ preset: 'custom', customStart: '2031-05-01' }), now, false],
  ])('computes upcoming visibility for %o', (value, clock, expected) => {
    expect(upcomingVisible(value, clock)).toBe(expected)
  })

  it('bounds custom upcoming events with a half-open interval and passes other selections through', () => {
    const custom = selection({
      preset: 'custom',
      customStart: '2031-05-10',
      customEnd: '2031-05-11',
    })
    const params = customRangeToParams(custom.customStart, custom.customEnd)
    const events = [
      { id: 'before', start_time: new Date(new Date(params.from).getTime() - 1).toISOString() },
      { id: 'from', start_time: params.from },
      { id: 'inside', start_time: new Date(new Date(params.from).getTime() + 1).toISOString() },
      { id: 'to', start_time: params.to },
      { id: 'after', start_time: new Date(new Date(params.to).getTime() + 1).toISOString() },
    ]
    expect(boundUpcomingEvents(events, custom).map(event => event.id)).toEqual(['from', 'inside'])
    expect(boundUpcomingEvents(events, selection({ preset: '30d' }))).toBe(events)
  })
})

describe('InteractionFilters', () => {
  const venues: VenueTagResponse[] = [
    { key: 'venue-a', label: 'Alpha', kind: 'group_chat', is_group: true },
    { key: 'venue-b', label: 'Beta', kind: 'email_thread', is_group: false },
  ]

  it('renders venue options in order and only shows date inputs for Custom', () => {
    const { rerender } = render(
      <InteractionFilters venueOptions={venues} selection={selection()} onChange={vi.fn()} />
    )
    const venue = screen.getByRole('combobox', { name: 'Venue' })
    expect(
      within(venue)
        .getAllByRole('option')
        .map(option => [option.textContent, option.getAttribute('value')])
    ).toEqual([
      ['All venues', ''],
      ['Alpha', 'venue-a'],
      ['Beta', 'venue-b'],
    ])
    expect(screen.getByRole('button', { name: 'All' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: '30 days' })).toHaveAttribute('aria-pressed', 'false')
    expect(screen.queryByLabelText('Start date')).not.toBeInTheDocument()
    rerender(
      <InteractionFilters
        venueOptions={venues}
        selection={selection({
          preset: 'custom',
          customStart: '2031-05-01',
          customEnd: '2031-05-02',
        })}
        onChange={vi.fn()}
      />
    )
    expect(screen.getByLabelText('Start date')).toHaveValue('2031-05-01')
    expect(screen.getByLabelText('End date')).toHaveValue('2031-05-02')
    expect(screen.getByRole('button', { name: 'Custom' })).toHaveAttribute('aria-pressed', 'true')
  })

  it('emits the exact next selection for venue, preset, and date changes', () => {
    const onChange = vi.fn()
    const { rerender } = render(
      <InteractionFilters
        venueOptions={venues}
        selection={selection({
          preset: 'custom',
          customStart: '2031-05-01',
          customEnd: '2031-05-02',
        })}
        onChange={onChange}
      />
    )
    fireEvent.change(screen.getByRole('combobox', { name: 'Venue' }), {
      target: { value: 'venue-b' },
    })
    expect(onChange).toHaveBeenLastCalledWith(
      selection({
        preset: 'custom',
        venue: 'venue-b',
        customStart: '2031-05-01',
        customEnd: '2031-05-02',
      })
    )
    rerender(
      <InteractionFilters
        venueOptions={venues}
        selection={selection({
          preset: 'custom',
          venue: 'venue-b',
          customStart: '2031-05-01',
          customEnd: '2031-05-02',
        })}
        onChange={onChange}
      />
    )
    fireEvent.click(screen.getByRole('button', { name: '30 days' }))
    expect(onChange).toHaveBeenLastCalledWith(
      selection({
        preset: '30d',
        venue: 'venue-b',
        customStart: '2031-05-01',
        customEnd: '2031-05-02',
      })
    )
    fireEvent.click(screen.getByRole('button', { name: 'Custom' }))
    expect(onChange).toHaveBeenLastCalledWith(
      selection({
        preset: 'custom',
        venue: 'venue-b',
        customStart: '2031-05-01',
        customEnd: '2031-05-02',
      })
    )
    fireEvent.change(screen.getByLabelText('Start date'), { target: { value: '2031-05-03' } })
    expect(onChange).toHaveBeenLastCalledWith(
      selection({
        preset: 'custom',
        venue: 'venue-b',
        customStart: '2031-05-03',
        customEnd: '2031-05-02',
      })
    )
  })
})
