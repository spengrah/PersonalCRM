import { apiClient } from './api-client'
import type {
  ContactTask,
  CreateActionTaskRequest,
  ContactTaskListParams,
} from '@/types/contact-task'

export const contactTasksApi = {
  // List tasks for a contact
  async listTasks(contactId: string, params: ContactTaskListParams = {}): Promise<ContactTask[]> {
    return apiClient.get<ContactTask[]>(`/api/v1/contacts/${contactId}/tasks`, params)
  },

  // Create an action task
  async createTask(contactId: string, data: CreateActionTaskRequest): Promise<ContactTask> {
    return apiClient.post<ContactTask>(`/api/v1/contacts/${contactId}/tasks`, data)
  },

  // Delete task link (remove CRM tracking)
  async deleteTaskLink(contactId: string, taskId: string): Promise<void> {
    return apiClient.delete<void>(`/api/v1/contacts/${contactId}/tasks/${taskId}`)
  },
}
