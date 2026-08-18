import { useQuery } from '@tanstack/react-query'
import { syncApi } from '@/lib/sync-api'
import { syncKeys } from '@/lib/query-keys'
import type { SyncState } from '@/types/sync'
import { staleTime } from '@/lib/query-client'

// Re-export for backwards compatibility
export { syncKeys }

// Get all sync states
// Note: Sync trigger returns 202 immediately while sync runs in background.
// Polling at 3s while syncing provides reasonable UX for typical syncs (10-60s).
// Error states may have up to 3s delay before appearing in UI, which is acceptable
// since sync errors are not time-critical and users can retry.
export function useSyncStates() {
  return useQuery({
    queryKey: syncKeys.states(),
    queryFn: () => syncApi.getSyncStates(),
    staleTime: staleTime(1000 * 5), // 5 seconds
    refetchInterval: query => {
      // Poll faster (3s) when any sync is in progress, slower (30s) when idle
      const states = query.state.data as SyncState[] | undefined
      const hasSyncing = states?.some(s => s.status === 'syncing')
      return hasSyncing ? 1000 * 3 : 1000 * 30
    },
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

// Auth error patterns that indicate reconnection is needed
const AUTH_ERROR_PATTERNS = [
  'invalid_grant',
  'token has been expired',
  'token has been revoked',
  'oauth',
  'authentication',
  'unauthorized',
] as const

// Check if an account needs reconnection due to auth errors
// Returns false if the account was updated more recently than the sync error (already reconnected)
export function accountNeedsReconnection(
  syncStates: SyncState[] | undefined,
  accountId: string,
  accountUpdatedAt: string
): boolean {
  if (!syncStates) return false

  const accountStates = syncStates.filter(s => s.account_id === accountId)
  const accountUpdatedTime = new Date(accountUpdatedAt).getTime()

  return accountStates.some(state => {
    if (state.status !== 'error' || !state.error_message) return false

    // If account was updated after the sync state error, credentials have been refreshed
    const syncStateUpdatedAt = new Date(state.updated_at).getTime()
    if (accountUpdatedTime > syncStateUpdatedAt) return false

    const error = state.error_message.toLowerCase()
    return AUTH_ERROR_PATTERNS.some(pattern => error.includes(pattern))
  })
}
