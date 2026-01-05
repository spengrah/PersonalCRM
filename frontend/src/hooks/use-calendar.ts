import { useQuery } from '@tanstack/react-query'
import { calendarApi } from '@/lib/calendar-api'

// Query key factory for calendar events
export const calendarKeys = {
  all: ['calendar-events'] as const,
  forContact: (contactId: string) => [...calendarKeys.all, 'contact', contactId] as const,
  upcomingForContact: (contactId: string) =>
    [...calendarKeys.all, 'upcoming', 'contact', contactId] as const,
  upcoming: () => [...calendarKeys.all, 'upcoming'] as const,
}

// Get all events for a contact
export function useEventsForContact(
  contactId: string,
  params?: { limit?: number; offset?: number }
) {
  return useQuery({
    queryKey: calendarKeys.forContact(contactId),
    queryFn: () => calendarApi.getEventsForContact(contactId, params),
    enabled: !!contactId,
    staleTime: 1000 * 60 * 5, // 5 minutes
  })
}

// Get upcoming events for a contact
export function useUpcomingEventsForContact(contactId: string, limit?: number) {
  return useQuery({
    queryKey: calendarKeys.upcomingForContact(contactId),
    queryFn: () => calendarApi.getUpcomingEventsForContact(contactId, limit),
    enabled: !!contactId,
    staleTime: 1000 * 60 * 5, // 5 minutes
  })
}

// Get all upcoming events with CRM contacts
export function useUpcomingEvents(params?: { limit?: number; offset?: number }) {
  return useQuery({
    queryKey: calendarKeys.upcoming(),
    queryFn: () => calendarApi.getUpcomingEvents(params),
    staleTime: 1000 * 60 * 5, // 5 minutes
  })
}
