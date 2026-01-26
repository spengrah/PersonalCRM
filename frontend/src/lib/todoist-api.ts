import { apiClient } from './api-client'

// Types
export interface TodoistSettings {
  project_id?: string
  label_id?: string
  label_name?: string
  integration_instance_id?: string
  user_timezone?: string
}

export interface TodoistSettingsUpdateRequest {
  project_id?: string
  label_id?: string
}

export interface TodoistProject {
  id: string
  name: string
}

export interface TodoistLabel {
  id: string
  name: string
}

// API functions
export const todoistApi = {
  /**
   * Get current Todoist integration settings
   */
  getSettings: async (): Promise<TodoistSettings> => {
    return apiClient.get<TodoistSettings>('/api/v1/todoist/settings')
  },

  /**
   * Update Todoist integration settings
   */
  updateSettings: async (settings: TodoistSettingsUpdateRequest): Promise<TodoistSettings> => {
    return apiClient.patch<TodoistSettings>('/api/v1/todoist/settings', settings)
  },

  /**
   * List all Todoist projects for the connected account
   */
  listProjects: async (): Promise<TodoistProject[]> => {
    return apiClient.get<TodoistProject[]>('/api/v1/todoist/projects')
  },

  /**
   * List all Todoist labels for the connected account
   */
  listLabels: async (): Promise<TodoistLabel[]> => {
    return apiClient.get<TodoistLabel[]>('/api/v1/todoist/labels')
  },
}
