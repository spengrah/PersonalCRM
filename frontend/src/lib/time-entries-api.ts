import { apiClient } from './api-client'
import type {
  TimeEntry,
  TimeEntryStats,
  CreateTimeEntryRequest,
  UpdateTimeEntryRequest,
  ListTimeEntriesParams,
} from '@/types/time-entry'

export async function createTimeEntry(data: CreateTimeEntryRequest): Promise<TimeEntry> {
  return await apiClient.post<TimeEntry>('/api/v1/time-entries', data)
}

export async function getTimeEntry(id: string): Promise<TimeEntry> {
  return await apiClient.get<TimeEntry>(`/api/v1/time-entries/${id}`)
}

export async function listTimeEntries(params?: ListTimeEntriesParams): Promise<TimeEntry[]> {
  const queryParams: Record<string, string> = {}
  if (params?.page) queryParams.page = params.page.toString()
  if (params?.limit) queryParams.limit = params.limit.toString()
  if (params?.contact_id) queryParams.contact_id = params.contact_id
  if (params?.start_date) queryParams.start_date = params.start_date
  if (params?.end_date) queryParams.end_date = params.end_date

  return await apiClient.get<TimeEntry[]>('/api/v1/time-entries', queryParams)
}

export async function getRunningTimeEntry(): Promise<TimeEntry | null> {
  try {
    return await apiClient.get<TimeEntry>('/api/v1/time-entries/running')
  } catch (error: any) {
    if (error.status === 404) {
      return null
    }
    throw error
  }
}

export async function updateTimeEntry(
  id: string,
  data: UpdateTimeEntryRequest
): Promise<TimeEntry> {
  return await apiClient.put<TimeEntry>(`/api/v1/time-entries/${id}`, data)
}

export async function deleteTimeEntry(id: string): Promise<void> {
  await apiClient.delete(`/api/v1/time-entries/${id}`)
}

export async function getTimeEntryStats(): Promise<TimeEntryStats> {
  return await apiClient.get<TimeEntryStats>('/api/v1/time-entries/stats')
}
