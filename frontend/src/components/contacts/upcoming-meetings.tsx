'use client'

import { Calendar, Clock, MapPin, Users } from 'lucide-react'
import { useUpcomingEventsForContact } from '@/hooks/use-calendar'

interface UpcomingMeetingsProps {
  contactId: string
}

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

export function UpcomingMeetings({ contactId }: UpcomingMeetingsProps) {
  const { data: events, isLoading, error } = useUpcomingEventsForContact(contactId, 5)

  if (isLoading) {
    return (
      <div className="bg-white shadow overflow-hidden sm:rounded-lg">
        <div className="px-4 py-5 sm:px-6 border-b border-gray-200">
          <h3 className="text-lg leading-6 font-medium text-gray-900 flex items-center">
            <Calendar className="w-5 h-5 mr-2 text-gray-400" />
            Upcoming Meetings
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
    return null // Don't show error - calendar sync may not be enabled
  }

  if (!events || events.length === 0) {
    return null // Don't show empty section
  }

  return (
    <div className="bg-white shadow overflow-hidden sm:rounded-lg">
      <div className="px-4 py-5 sm:px-6 border-b border-gray-200">
        <h3 className="text-lg leading-6 font-medium text-gray-900 flex items-center">
          <Calendar className="w-5 h-5 mr-2 text-gray-400" />
          Upcoming Meetings ({events.length})
        </h3>
        <p className="mt-1 max-w-2xl text-sm text-gray-500">
          Scheduled meetings from your calendar
        </p>
      </div>
      <div className="divide-y divide-gray-200">
        {events.map(event => (
          <div key={event.id} className="px-4 py-4 sm:px-6">
            <div className="flex items-start justify-between">
              <div className="flex-1 min-w-0">
                <h4 className="text-sm font-medium text-gray-900 truncate">
                  {event.title || 'Untitled Meeting'}
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
        ))}
      </div>
    </div>
  )
}
