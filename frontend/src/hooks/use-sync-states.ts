import { useQuery } from '@tanstack/react-query'
import { syncApi } from '@/lib/sync-api'
import type { SyncState } from '@/types/sync'

// Query keys for sync states
export const syncKeys = {
  all: ['sync'] as const,
  states: () => [...syncKeys.all, 'states'] as const,
  state: (source: string, accountId?: string) =>
    [...syncKeys.all, 'state', source, accountId] as const,
}

// Get all sync states
export function useSyncStates() {
  return useQuery({
    queryKey: syncKeys.states(),
    queryFn: () => syncApi.getSyncStates(),
    staleTime: 1000 * 30, // 30 seconds
    refetchInterval: 1000 * 60, // Refetch every minute
  })
}

// Helper to get sync state for a specific source and account
export function getSyncStateForAccount(
  states: SyncState[] | undefined,
  source: string,
  accountId: string
): SyncState | undefined {
  return states?.find(s => s.source === source && s.account_id === accountId)
}

// Format relative time for sync status
export function formatSyncTime(dateString: string | null): string {
  if (!dateString) return 'Never'

  const date = new Date(dateString)
  const now = new Date()
  const diffMs = now.getTime() - date.getTime()
  const diffMins = Math.floor(diffMs / (1000 * 60))
  const diffHours = Math.floor(diffMs / (1000 * 60 * 60))
  const diffDays = Math.floor(diffMs / (1000 * 60 * 60 * 24))

  if (diffMins < 1) return 'Just now'
  if (diffMins < 60) return `${diffMins}m ago`
  if (diffHours < 24) return `${diffHours}h ago`
  if (diffDays < 7) return `${diffDays}d ago`

  return date.toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
  })
}
