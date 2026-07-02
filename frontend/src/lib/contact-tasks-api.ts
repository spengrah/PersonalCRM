import { apiClient } from './api-client'
import type {
  ContactTask,
  CreateManualTaskRequest,
  ContactTaskListParams,
} from '@/types/contact-task'

export const contactTasksApi = {
  // List tasks for a contact
  async listTasks(contactId: string, params: ContactTaskListParams = {}): Promise<ContactTask[]> {
    // Spread into a fresh object literal: interfaces lack the implicit index
    // signature that apiClient's Record<string, unknown> params require.
    return apiClient.get<ContactTask[]>(`/api/v1/contacts/${contactId}/tasks`, { ...params })
  },

  // Create a manual (user-picker) task — kind in {reach_out, send, reminder}.
  async createTask(contactId: string, data: CreateManualTaskRequest): Promise<ContactTask> {
    return apiClient.post<ContactTask>(`/api/v1/contacts/${contactId}/tasks`, data)
  },

  // Delete task link (remove CRM tracking)
  async deleteTaskLink(contactId: string, taskId: string): Promise<void> {
    return apiClient.delete<void>(`/api/v1/contacts/${contactId}/tasks/${taskId}`)
  },
}
