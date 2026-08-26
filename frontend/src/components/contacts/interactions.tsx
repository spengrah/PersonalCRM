'use client'

import { useState } from 'react'
import { useContactInteractions } from '@/hooks/use-interactions'
import { useUpcomingEventsForContact } from '@/hooks/use-calendar'
import { useAcceleratedTime } from '@/hooks/use-accelerated-time'
import type { CalendarEvent } from '@/types/calendar'
import { InteractionRow } from './interaction-row'
import {
  InteractionFilters,
  DEFAULT_FILTER_SELECTION,
  boundUpcomingEvents,
  selectionToFilters,
  upcomingVisible,
  type InteractionFilterSelection,
} from './interaction-filters'
import type { InteractionListFilters } from '@/lib/interactions-api'

const dateTimeFormatter = new Intl.DateTimeFormat('en-US', {
  weekday: 'short',
  month: 'short',
  day: 'numeric',
  hour: 'numeric',
  minute: '2-digit',
})
const timeFormatter = new Intl.DateTimeFormat('en-US', { hour: 'numeric', minute: '2-digit' })

function UpcomingCard({ event }: { event: CalendarEvent }) {
  return (
    <article role="listitem" data-event-id={event.id} className="px-4 py-4 sm:px-6">
      <div className="flex items-start gap-3">
        <span className="rounded bg-green-100 px-2 py-0.5 text-xs font-medium text-green-800">
          Upcoming
        </span>
        <div className="min-w-0 flex-1">
          <div className="font-medium text-gray-900">
            {event.html_link ? (
              <a
                href={event.html_link}
                target="_blank"
                rel="noopener noreferrer"
                className="text-blue-600 hover:underline"
              >
                {event.title}
              </a>
            ) : (
              event.title
            )}
          </div>
          <div className="mt-1 text-sm text-gray-600">
            {dateTimeFormatter.format(new Date(event.start_time))}
          </div>
          <div className="text-sm text-gray-600">
            {timeFormatter.format(new Date(event.start_time))} -{' '}
            {timeFormatter.format(new Date(event.end_time))}
          </div>
          {event.location && <div className="text-sm text-gray-600">{event.location}</div>}
          {event.attendee_count > 1 && (
            <div className="text-sm text-gray-600">{event.attendee_count} attendees</div>
          )}
        </div>
      </div>
    </article>
  )
}

export function Interactions({ contactId }: { contactId: string }) {
  const [showAllUpcoming, setShowAllUpcoming] = useState(false)
  const [selection, setSelection] = useState(DEFAULT_FILTER_SELECTION)
  const [filters, setFilters] = useState<InteractionListFilters>({})
  const { currentTime } = useAcceleratedTime()
  const applySelection = (next: InteractionFilterSelection) => {
    setSelection(next)
    setFilters(selectionToFilters(next, currentTime))
  }
  const interactions = useContactInteractions(contactId, filters)
  const upcoming = useUpcomingEventsForContact(contactId, 250)
  const items = interactions.data?.pages.flatMap(page => page.data?.items ?? []) ?? []
  const events = upcoming.data ?? []
  const venueOptions = interactions.data?.pages[0]?.data?.venue_options ?? []
  const showUpcoming = upcomingVisible(selection, currentTime)
  const boundedEvents = showUpcoming ? boundUpcomingEvents(events, selection) : []
  const displayedEvents = showAllUpcoming ? boundedEvents : boundedEvents.slice(0, 3)

  return (
    <section aria-label="Interactions" className="bg-white shadow overflow-hidden sm:rounded-lg">
      <div className="border-b border-gray-200 px-4 py-5 sm:px-6">
        <h3 className="text-lg font-medium leading-6 text-gray-900">Interactions</h3>
      </div>
      <InteractionFilters
        venueOptions={venueOptions}
        selection={selection}
        onChange={applySelection}
      />
      {boundedEvents.length > 0 && (
        <div>
          <div role="list" aria-label="Upcoming events" className="divide-y divide-gray-200">
            {displayedEvents.map(event => (
              <UpcomingCard key={event.id} event={event} />
            ))}
          </div>
          {!showAllUpcoming && boundedEvents.length > 3 && (
            <div className="border-t border-gray-200 px-4 py-4 sm:px-6">
              <button
                type="button"
                onClick={() => setShowAllUpcoming(true)}
                className="w-full rounded border border-gray-300 px-3 py-2 text-sm font-medium text-gray-700"
              >
                Show all {boundedEvents.length} upcoming
              </button>
            </div>
          )}
        </div>
      )}
      {interactions.error ? (
        <p className="border-t border-gray-200 p-6 text-sm text-red-700">
          Failed to load interactions.
        </p>
      ) : items.length === 0 &&
        !interactions.isLoading &&
        !upcoming.isLoading &&
        !upcoming.error &&
        boundedEvents.length === 0 ? (
        <p className="border-t border-gray-200 p-6 text-sm text-gray-500">
          No interactions recorded for this contact.
        </p>
      ) : (
        <>
          {items.length > 0 && (
            <div role="list" aria-label="Interaction history" className="divide-y divide-gray-200">
              {items.map(item => (
                <InteractionRow
                  key={item.id}
                  item={item}
                  venueFilter={selection.venue || undefined}
                />
              ))}
            </div>
          )}
          {interactions.hasNextPage && (
            <div className="border-t border-gray-200 px-4 py-4 sm:px-6">
              <button
                type="button"
                onClick={() => interactions.fetchNextPage()}
                disabled={interactions.isFetchingNextPage}
                className="w-full rounded border border-gray-300 px-3 py-2 text-sm font-medium text-gray-700 disabled:opacity-50"
              >
                Load more
              </button>
            </div>
          )}
        </>
      )}
    </section>
  )
}
