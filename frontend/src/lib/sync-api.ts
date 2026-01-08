import { apiClient } from './api-client'
import type { SyncState } from '@/types/sync'

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
}
