// Calendar event types

export interface CalendarEvent {
  id: string
  title: string
  description?: string
  location?: string
  start_time: string
  end_time: string
  status: string
  attendee_count: number
}
