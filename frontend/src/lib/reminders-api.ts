import { apiClient } from './api-client'
import type {
  Reminder,
  DueReminder,
  CreateReminderRequest,
  ReminderListParams,
  ReminderStats,
} from '@/types/reminder'

export interface RemindersListResponse {
  reminders: DueReminder[]
  total: number
  page: number
  limit: number
  pages: number
}

export const remindersApi = {
  // Get all reminders
  getReminders: async (params: ReminderListParams = {}): Promise<DueReminder[]> => {
    const queryParams = {
      page: params.page || 1,
      limit: params.limit || 20,
      ...(params.due_today !== undefined && { due_today: params.due_today }),
    }

    return apiClient.get<DueReminder[]>('/api/v1/reminders', queryParams)
  },

  // Get reminders for a specific contact
  getRemindersByContact: async (contactId: string): Promise<Reminder[]> => {
    return apiClient.get<Reminder[]>(`/api/v1/contacts/${contactId}/reminders`)
  },

  // Create reminder
  createReminder: async (data: CreateReminderRequest): Promise<Reminder> => {
    return apiClient.post<Reminder>('/api/v1/reminders', data)
  },

  // Complete reminder
  completeReminder: async (id: string): Promise<Reminder> => {
    return apiClient.patch<Reminder>(`/api/v1/reminders/${id}/complete`)
  },

  // Delete reminder
  deleteReminder: async (id: string): Promise<void> => {
    return apiClient.delete<void>(`/api/v1/reminders/${id}`)
  },

  // Get reminder statistics
  getStats: async (): Promise<ReminderStats> => {
    return apiClient.get<ReminderStats>('/api/v1/reminders/stats')
  },
}
