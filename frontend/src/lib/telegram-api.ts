import { apiClient } from './api-client'

export interface TelegramAuthStartRequest {
  phone_number: string
}

export interface TelegramAuthStartResponse {
  auth_token: string
  status: string
  code_type: string
  expires_in: number
}

export interface TelegramAuthVerifyCodeRequest {
  auth_token: string
  code: string
}

export interface TelegramAuthVerifyPasswordRequest {
  auth_token: string
  password: string
}

export interface TelegramAuthResponse {
  status: string
  username?: string
  user_id?: number
}

export interface TelegramStatus {
  status: string
  connected: boolean
  username?: string
  phone_number?: string
  last_sync_at?: string
  connected_at?: string
  error?: string
}

export const telegramApi = {
  startAuth: (data: TelegramAuthStartRequest) =>
    apiClient.post<TelegramAuthStartResponse>('/api/v1/telegram/auth/start', data),

  verifyCode: (data: TelegramAuthVerifyCodeRequest) =>
    apiClient.post<TelegramAuthResponse>('/api/v1/telegram/auth/verify-code', data),

  verifyPassword: (data: TelegramAuthVerifyPasswordRequest) =>
    apiClient.post<TelegramAuthResponse>('/api/v1/telegram/auth/verify-password', data),

  cancelAuth: () => apiClient.post<{ status: string }>('/api/v1/telegram/auth/cancel', {}),

  disconnect: () => apiClient.delete<{ status: string }>('/api/v1/telegram/auth'),

  getStatus: () => apiClient.get<TelegramStatus>('/api/v1/telegram/auth/status'),
}
