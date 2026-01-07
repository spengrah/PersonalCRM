'use client'

import { useState, useMemo } from 'react'
import { Calendar, Clock, MapPin, Users, ExternalLink, ChevronDown } from 'lucide-react'
import { useEventsForContact } from '@/hooks/use-calendar'
import { useAcceleratedTime } from '@/hooks/use-accelerated-time'
import { Button } from '@/components/ui/button'
import type { CalendarEvent } from '@/types/calendar'

interface MeetingsProps {
  contactId: string
}

type FilterType = 'all' | 'upcoming' | 'past'

function formatDateTime(dateString: string): string {
  const date = new Date(dateString)
  return new Intl.DateTimeFormat('en-US', {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  }).format(date)
}

function formatTimeRange(startTime: string, endTime: string): string {
  const start = new Date(startTime)
  const end = new Date(endTime)

  const startStr = new Intl.DateTimeFormat('en-US', {
    hour: 'numeric',
    minute: '2-digit',
  }).format(start)

  const endStr = new Intl.DateTimeFormat('en-US', {
    hour: 'numeric',
    minute: '2-digit',
  }).format(end)

  return `${startStr} - ${endStr}`
}

function isPastEvent(event: CalendarEvent, currentTime: Date): boolean {
  return new Date(event.end_time) < currentTime
}

function MeetingCard({ event, isPast }: { event: CalendarEvent; isPast: boolean }) {
  const title = event.title || 'Untitled Meeting'

  return (
    <div className={`px-4 py-4 sm:px-6 ${isPast ? 'opacity-60' : ''}`}>
      <div className="flex items-start justify-between">
        <div className="flex-1 min-w-0">
          <h4 className="text-sm font-medium text-gray-900 truncate flex items-center gap-2">
            {event.html_link ? (
              <a
                href={event.html_link}
                target="_blank"
                rel="noopener noreferrer"
                className="hover:text-blue-600 hover:underline flex items-center gap-1"
              >
                {title}
                <ExternalLink className="w-3 h-3 flex-shrink-0" />
              </a>
            ) : (
              title
            )}
            {isPast && (
              <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-gray-100 text-gray-600">
                Past
              </span>
            )}
          </h4>
          <div className="mt-2 flex flex-col space-y-1 text-sm text-gray-500">
            <div className="flex items-center space-x-1">
              <Clock className="w-4 h-4 flex-shrink-0" />
              <span>{formatDateTime(event.start_time)}</span>
              <span className="text-gray-400">
                ({formatTimeRange(event.start_time, event.end_time)})
              </span>
            </div>
            {event.location && (
              <div className="flex items-center space-x-1">
                <MapPin className="w-4 h-4 flex-shrink-0" />
                <span className="truncate">{event.location}</span>
              </div>
            )}
            {event.attendee_count > 1 && (
              <div className="flex items-center space-x-1">
                <Users className="w-4 h-4 flex-shrink-0" />
                <span>{event.attendee_count} attendees</span>
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}

export function Meetings({ contactId }: MeetingsProps) {
  const [filter, setFilter] = useState<FilterType>('upcoming')
  const [displayLimit, setDisplayLimit] = useState(10)

  const { data: events, isLoading, error } = useEventsForContact(contactId, { limit: 100 })
  const { currentTime } = useAcceleratedTime()

  const { filteredEvents, upcomingCount, pastCount } = useMemo(() => {
    if (!events) return { filteredEvents: [], upcomingCount: 0, pastCount: 0 }

    const upcoming = events.filter(e => !isPastEvent(e, currentTime))
    const past = events.filter(e => isPastEvent(e, currentTime))

    let filtered: CalendarEvent[]
    switch (filter) {
      case 'upcoming':
        filtered = upcoming
        break
      case 'past':
        filtered = past.sort(
          (a, b) => new Date(b.start_time).getTime() - new Date(a.start_time).getTime()
        )
        break
      default:
        filtered = [
          ...upcoming,
          ...past.sort(
            (a, b) => new Date(b.start_time).getTime() - new Date(a.start_time).getTime()
          ),
        ]
    }

    return {
      filteredEvents: filtered,
      upcomingCount: upcoming.length,
      pastCount: past.length,
    }
  }, [events, filter, currentTime])

  const displayedEvents = filteredEvents.slice(0, displayLimit)
  const hasMore = filteredEvents.length > displayLimit

  if (isLoading) {
    return (
      <div className="bg-white shadow overflow-hidden sm:rounded-lg">
        <div className="px-4 py-5 sm:px-6 border-b border-gray-200">
          <h3 className="text-lg leading-6 font-medium text-gray-900 flex items-center">
            <Calendar className="w-5 h-5 mr-2 text-gray-400" />
            Meetings
          </h3>
        </div>
        <div className="p-6">
          <div className="animate-pulse space-y-4">
            <div className="h-4 bg-gray-200 rounded w-3/4"></div>
            <div className="h-4 bg-gray-200 rounded w-1/2"></div>
          </div>
        </div>
      </div>
    )
  }

  if (error) {
    return null
  }

  if (!events || events.length === 0) {
    return null
  }

  const filterButtons: { key: FilterType; label: string; count: number }[] = [
    { key: 'all', label: 'All', count: events.length },
    { key: 'upcoming', label: 'Upcoming', count: upcomingCount },
    { key: 'past', label: 'Past', count: pastCount },
  ]

  return (
    <div className="bg-white shadow overflow-hidden sm:rounded-lg">
      <div className="px-4 py-5 sm:px-6 border-b border-gray-200">
        <div className="flex items-center justify-between">
          <div>
            <h3 className="text-lg leading-6 font-medium text-gray-900 flex items-center">
              <Calendar className="w-5 h-5 mr-2 text-gray-400" />
              Meetings
            </h3>
            <p className="mt-1 max-w-2xl text-sm text-gray-500">
              Calendar events with this contact
            </p>
          </div>
          <div className="flex rounded-lg border border-gray-200 p-1">
            {filterButtons.map(({ key, label, count }) => (
              <button
                key={key}
                onClick={() => {
                  setFilter(key)
                  setDisplayLimit(10)
                }}
                className={`px-3 py-1.5 text-sm font-medium rounded-md transition-colors ${
                  filter === key
                    ? 'bg-gray-900 text-white'
                    : 'text-gray-600 hover:text-gray-900 hover:bg-gray-50'
                }`}
              >
                {label} ({count})
              </button>
            ))}
          </div>
        </div>
      </div>
      <div className="divide-y divide-gray-200">
        {displayedEvents.map(event => (
          <MeetingCard key={event.id} event={event} isPast={isPastEvent(event, currentTime)} />
        ))}
      </div>
      {hasMore && (
        <div className="px-4 py-4 sm:px-6 border-t border-gray-200">
          <Button
            variant="outline"
            size="sm"
            onClick={() => setDisplayLimit(prev => prev + 10)}
            className="w-full"
          >
            <ChevronDown className="w-4 h-4 mr-2" />
            Load more ({filteredEvents.length - displayLimit} remaining)
          </Button>
        </div>
      )}
    </div>
  )
}
