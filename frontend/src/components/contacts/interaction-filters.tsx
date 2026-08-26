import type { InteractionListFilters } from '@/lib/interactions-api'
import type { VenueTagResponse } from '@/types/generated/contact'

export interface InteractionFilterSelection {
  preset: 'all' | '30d' | '90d' | 'custom'
  venue: string
  customStart: string
  customEnd: string
}

export const DEFAULT_FILTER_SELECTION: InteractionFilterSelection = {
  preset: 'all',
  venue: '',
  customStart: '',
  customEnd: '',
}

function localDate(yearMonthDay: string, dayOffset = 0): Date {
  const [year, month, day] = yearMonthDay.split('-').map(Number)
  return new Date(year, month - 1, day + dayOffset)
}

export function customRangeToParams(start: string, end: string): { from: string; to: string } {
  return {
    from: localDate(start).toISOString(),
    to: localDate(end, 1).toISOString(),
  }
}

export function selectionToFilters(
  selection: InteractionFilterSelection,
  now: Date
): InteractionListFilters {
  const filters: InteractionListFilters = {}
  if (selection.venue) filters.venue = selection.venue

  if (selection.preset === '30d' || selection.preset === '90d') {
    const days = selection.preset === '30d' ? 30 : 90
    filters.from = new Date(now.getTime() - days * 24 * 60 * 60 * 1000).toISOString()
  } else if (selection.preset === 'custom' && selection.customStart && selection.customEnd) {
    Object.assign(filters, customRangeToParams(selection.customStart, selection.customEnd))
  }

  return filters
}

export function upcomingVisible(selection: InteractionFilterSelection, now: Date): boolean {
  if (selection.venue) return false
  if (selection.preset === 'all') return true
  if (selection.preset === '30d' || selection.preset === '90d') return false
  if (!selection.customStart || !selection.customEnd) return false
  return new Date(customRangeToParams(selection.customStart, selection.customEnd).to) > now
}

export function boundUpcomingEvents<E extends { start_time: string }>(
  events: E[],
  selection: InteractionFilterSelection
): E[] {
  if (selection.preset !== 'custom' || !selection.customStart || !selection.customEnd) {
    return events
  }
  const { from, to } = customRangeToParams(selection.customStart, selection.customEnd)
  const fromTime = new Date(from).getTime()
  const toTime = new Date(to).getTime()
  return events.filter(event => {
    const start = new Date(event.start_time).getTime()
    return start >= fromTime && start < toTime
  })
}

export function InteractionFilters({
  venueOptions,
  selection,
  onChange,
}: {
  venueOptions: VenueTagResponse[]
  selection: InteractionFilterSelection
  onChange: (next: InteractionFilterSelection) => void
}): React.JSX.Element {
  const setPreset = (preset: InteractionFilterSelection['preset']) =>
    onChange({ ...selection, preset })

  return (
    <div className="flex flex-wrap items-end gap-3 border-b border-gray-200 px-4 py-3 sm:px-6">
      <label className="flex flex-col gap-1 text-sm text-gray-700">
        <span>Venue</span>
        <select
          aria-label="Venue"
          value={selection.venue}
          onChange={event => onChange({ ...selection, venue: event.target.value })}
          className="rounded border border-gray-300 px-2 py-1"
        >
          <option value="">All venues</option>
          {venueOptions.map(tag => (
            <option key={tag.key} value={tag.key}>
              {tag.label}
            </option>
          ))}
        </select>
      </label>
      <div className="flex flex-wrap gap-2" role="group" aria-label="Interaction date range">
        {(
          [
            ['all', 'All'],
            ['30d', '30 days'],
            ['90d', '90 days'],
            ['custom', 'Custom'],
          ] as const
        ).map(([preset, label]) => (
          <button
            key={preset}
            type="button"
            aria-pressed={selection.preset === preset}
            onClick={() => setPreset(preset)}
            className="rounded border border-gray-300 px-3 py-1 text-sm text-gray-700"
          >
            {label}
          </button>
        ))}
      </div>
      {selection.preset === 'custom' && (
        <div className="flex gap-2">
          <label className="flex flex-col gap-1 text-sm text-gray-700">
            <span>Start date</span>
            <input
              type="date"
              aria-label="Start date"
              value={selection.customStart}
              onChange={event => onChange({ ...selection, customStart: event.target.value })}
              className="rounded border border-gray-300 px-2 py-1"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm text-gray-700">
            <span>End date</span>
            <input
              type="date"
              aria-label="End date"
              value={selection.customEnd}
              onChange={event => onChange({ ...selection, customEnd: event.target.value })}
              className="rounded border border-gray-300 px-2 py-1"
            />
          </label>
        </div>
      )}
    </div>
  )
}
