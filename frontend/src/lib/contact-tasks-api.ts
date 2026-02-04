import type {
  ContactTask,
  CreateActionTaskRequest,
  ContactTaskListParams,
} from '@/types/contact-task'

const API_BASE = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080/api/v1'

async function fetchWithAuth(url: string, options: RequestInit = {}) {
  const apiKey = process.env.NEXT_PUBLIC_API_KEY || ''
  const response = await fetch(url, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${apiKey}`,
      ...options.headers,
    },
  })

  if (!response.ok) {
    const error = await response.json().catch(() => ({ error: { message: 'Unknown error' } }))
    throw new Error(error.error?.message || `Request failed: ${response.status}`)
  }

  // Handle 204 No Content
  if (response.status === 204) {
    return null
  }

  return response.json()
}

export const contactTasksApi = {
  // List tasks for a contact
  async listTasks(contactId: string, params: ContactTaskListParams = {}): Promise<ContactTask[]> {
    const searchParams = new URLSearchParams()
    if (params.state) searchParams.set('state', params.state)
    if (params.kind) searchParams.set('kind', params.kind)
    const queryString = searchParams.toString()
    const url = `${API_BASE}/contacts/${contactId}/tasks${queryString ? `?${queryString}` : ''}`
    const response = await fetchWithAuth(url)
    return response.data
  },

  // Create an action task
  async createTask(contactId: string, data: CreateActionTaskRequest): Promise<ContactTask> {
    const response = await fetchWithAuth(`${API_BASE}/contacts/${contactId}/tasks`, {
      method: 'POST',
      body: JSON.stringify(data),
    })
    return response.data
  },

  // Delete task link (remove CRM tracking)
  async deleteTaskLink(contactId: string, taskId: string): Promise<void> {
    await fetchWithAuth(`${API_BASE}/contacts/${contactId}/tasks/${taskId}`, {
      method: 'DELETE',
    })
  },
}
