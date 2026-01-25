import { apiClient } from './api-client'

// Types
export interface GoogleAuthURLResponse {
  url: string
  state: string
}

export interface GoogleAccount {
  id: string
  account_id: string
  account_name?: string
  expires_at?: string
  scopes?: string[]
  created_at: string
  updated_at: string
}

export interface TodoistAuthURLResponse {
  url: string
  state: string
}

export interface TodoistAccount {
  id: string
  account_id: string
  account_name?: string
  expires_at?: string
  scopes?: string[]
  created_at: string
  updated_at: string
}

// API functions
export const oauthApi = {
  /**
   * Get Google OAuth authorization URL
   * Returns the URL to redirect the user to for Google authorization
   */
  getGoogleAuthUrl: async (): Promise<GoogleAuthURLResponse> => {
    return apiClient.get<GoogleAuthURLResponse>('/api/v1/auth/google')
  },

  /**
   * List all connected Google accounts
   */
  listGoogleAccounts: async (): Promise<GoogleAccount[]> => {
    return apiClient.get<GoogleAccount[]>('/api/v1/auth/google/accounts')
  },

  /**
   * Get status of a specific Google account
   */
  getGoogleAccountStatus: async (id: string): Promise<GoogleAccount> => {
    return apiClient.get<GoogleAccount>(`/api/v1/auth/google/accounts/${id}/status`)
  },

  /**
   * Revoke (disconnect) a Google account
   */
  revokeGoogleAccount: async (id: string): Promise<{ message: string }> => {
    return apiClient.post<{ message: string }>(`/api/v1/auth/google/accounts/${id}/revoke`)
  },

  /**
   * Get Todoist OAuth authorization URL
   * Returns the URL to redirect the user to for Todoist authorization
   */
  getTodoistAuthUrl: async (): Promise<TodoistAuthURLResponse> => {
    return apiClient.get<TodoistAuthURLResponse>('/api/v1/auth/todoist')
  },

  /**
   * List all connected Todoist accounts
   */
  listTodoistAccounts: async (): Promise<TodoistAccount[]> => {
    return apiClient.get<TodoistAccount[]>('/api/v1/auth/todoist/accounts')
  },

  /**
   * Get status of a specific Todoist account
   */
  getTodoistAccountStatus: async (id: string): Promise<TodoistAccount> => {
    return apiClient.get<TodoistAccount>(`/api/v1/auth/todoist/accounts/${id}/status`)
  },

  /**
   * Revoke (disconnect) a Todoist account
   */
  revokeTodoistAccount: async (id: string): Promise<{ message: string }> => {
    return apiClient.post<{ message: string }>(`/api/v1/auth/todoist/accounts/${id}/revoke`)
  },
}

/**
 * Start the Google OAuth flow by opening the authorization URL
 * This redirects the user to Google's consent screen
 */
export async function startGoogleOAuthFlow(): Promise<void> {
  const { url } = await oauthApi.getGoogleAuthUrl()
  // Redirect to Google's authorization page
  window.location.href = url
}

/**
 * Start the Todoist OAuth flow by opening the authorization URL
 * This redirects the user to Todoist's consent screen
 */
export async function startTodoistOAuthFlow(): Promise<void> {
  const { url } = await oauthApi.getTodoistAuthUrl()
  // Redirect to Todoist's authorization page
  window.location.href = url
}
