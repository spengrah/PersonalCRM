import { useQuery } from '@tanstack/react-query'
import { syncApi } from '@/lib/sync-api'
import { syncKeys } from '@/lib/query-keys'
import type { SyncState } from '@/types/sync'

// Re-export for backwards compatibility
export { syncKeys }

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

// Aggregate sync status type
export type AggregateSyncStatus = 'synced' | 'syncing' | 'error'

// Get aggregate sync status across all sync states
// Priority: syncing > error > synced
export function getAggregateSyncStatus(states: SyncState[] | undefined): AggregateSyncStatus {
  if (!states || states.length === 0) return 'synced'

  const hasSyncing = states.some(s => s.status === 'syncing')
  const hasError = states.some(s => s.status === 'error')

  if (hasSyncing) return 'syncing'
  if (hasError) return 'error'
  return 'synced'
}

// Get icon classes for sync status indicator on Imports nav item
export function getSyncIconClasses(syncStatus: AggregateSyncStatus): string {
  switch (syncStatus) {
    case 'syncing':
      return 'text-green-600 animate-sync-pulse'
    case 'error':
      return 'text-red-700'
    default:
      return '' // Use default text color from parent
  }
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
