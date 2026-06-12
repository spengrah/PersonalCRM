import { apiClient } from './api-client'
import type { StalenessBreach, SyncState } from '@/types/sync'

export const syncApi = {
  // Get all sync states
  getSyncStates: async (): Promise<SyncState[]> => {
    return apiClient.get<SyncState[]>('/api/v1/sync/status')
  },

  // Get sync state for a specific source
  getSyncState: async (source: string, accountId?: string): Promise<SyncState> => {
    const params = accountId ? `?account_id=${encodeURIComponent(accountId)}` : ''
    return apiClient.get<SyncState>(`/api/v1/sync/${source}/status${params}`)
  },

  // Get active sync-staleness breaches reported by the watchdog.
  getStalenessBreaches: async (): Promise<StalenessBreach[]> => {
    return apiClient.get<StalenessBreach[]>('/api/v1/sync/staleness')
  },
}
