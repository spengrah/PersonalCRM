import { apiClient } from './api-client'
import type { SystemTimeResponse, SetAccelerationRequest } from '@/types/system'

export const systemApi = {
  // Get current system time (potentially accelerated)
  getSystemTime: async (): Promise<SystemTimeResponse> => {
    // Handle the inconsistent API response format
    const response = await fetch('http://localhost:8080/api/v1/system/time')
    if (!response.ok) {
      throw new Error(`HTTP error! status: ${response.status}`)
    }
    const data = await response.json()
    
    // Handle both wrapped ({"success": true, "data": {...}}) and direct formats
    return data.data || data
  },

  // Set time acceleration for testing
  setTimeAcceleration: async (settings: SetAccelerationRequest): Promise<void> => {
    return apiClient.post<void>('/api/v1/system/time/acceleration', settings)
  },
}

