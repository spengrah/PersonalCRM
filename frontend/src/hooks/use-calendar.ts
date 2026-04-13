import { useQuery } from '@tanstack/react-query'
import { calendarApi } from '@/lib/calendar-api'
import { calendarKeys } from '@/lib/query-keys'

// Re-export for backward compatibility (key was previously defined here).
export { calendarKeys }

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
