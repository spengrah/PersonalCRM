export interface TimeEntry {
  id: string
  description: string
  project?: string
  contact_id?: string
  start_time: string
  end_time?: string
  duration_minutes?: number
  created_at: string
  updated_at: string
}

export interface TimeEntryStats {
  total_entries: number
  total_minutes: number
  today_minutes: number
  week_minutes: number
  month_minutes: number
}

export interface CreateTimeEntryRequest {
  description: string
  project?: string
  contact_id?: string
  start_time: string
  end_time?: string
  duration_minutes?: number
}

export interface UpdateTimeEntryRequest {
  description?: string
  project?: string
  contact_id?: string
  end_time?: string
  duration_minutes?: number
}

export interface ListTimeEntriesParams {
  page?: number
  limit?: number
  contact_id?: string
  start_date?: string
  end_date?: string
}
