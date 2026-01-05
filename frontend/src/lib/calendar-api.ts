import { apiClient } from './api-client'
import type { CalendarEvent } from '@/types/calendar'

export const calendarApi = {
  // Get all events for a contact
  getEventsForContact: async (
    contactId: string,
    params?: { limit?: number; offset?: number }
  ): Promise<CalendarEvent[]> => {
    return apiClient.get<CalendarEvent[]>(`/api/v1/contacts/${contactId}/events`, params)
  },

  // Get upcoming events for a contact
  getUpcomingEventsForContact: async (
    contactId: string,
    limit?: number
  ): Promise<CalendarEvent[]> => {
    return apiClient.get<CalendarEvent[]>(`/api/v1/contacts/${contactId}/events/upcoming`, {
      limit,
    })
  },

  // Get all upcoming events with CRM contacts
  getUpcomingEvents: async (params?: {
    limit?: number
    offset?: number
  }): Promise<CalendarEvent[]> => {
    return apiClient.get<CalendarEvent[]>('/api/v1/events/upcoming', params)
  },
}
