import { apiClient } from './api-client'
import type { SystemTimeResponse, SetAccelerationRequest } from '@/types/system'

export const systemApi = {
  // Get current system time (potentially accelerated)
  getSystemTime: async (): Promise<SystemTimeResponse> => {
    return apiClient.get<SystemTimeResponse>('/api/v1/system/time')
  },

  // Set time acceleration for testing
  setTimeAcceleration: async (settings: SetAccelerationRequest): Promise<void> => {
    return apiClient.post<void>('/api/v1/system/time/acceleration', settings)
  },
}

