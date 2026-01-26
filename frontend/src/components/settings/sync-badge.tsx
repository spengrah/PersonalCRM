'use client'

import { RefreshCw } from 'lucide-react'
import type { SyncState } from '@/types/sync'
import { formatSyncTime } from '@/hooks/use-sync-states'

interface SyncBadgeProps {
  label: string
  syncState?: SyncState
  onSync: () => void
  loading: boolean
}

/**
 * Shared sync badge component for OAuth account sections.
 * Shows label | sync status | refresh button in a segmented badge.
 *
 * Matches the design in designs/ui-components.pen (SyncBadge component).
 */
export function SyncBadge({ label, syncState, onSync, loading }: SyncBadgeProps) {
  const lastSyncText = formatSyncTime(syncState?.last_successful_sync_at ?? null)
  const isSyncing = syncState?.status === 'syncing'
  const hasError = syncState?.status === 'error'

  return (
    <div className="inline-flex items-center rounded-md text-xs font-medium bg-white border border-gray-200 overflow-hidden">
      <span className="px-2.5 py-1 text-gray-700 border-r border-gray-200">{label}</span>
      <span className={`px-2 py-1 bg-gray-50 ${hasError ? 'text-red-600' : 'text-gray-500'}`}>
        {isSyncing ? 'Syncing...' : hasError ? 'Error' : lastSyncText}
      </span>
      <button
        onClick={onSync}
        disabled={loading || isSyncing}
        className="px-2 py-1 text-blue-600 hover:bg-blue-50 border-l border-gray-200 disabled:opacity-50"
      >
        <RefreshCw className={`w-3 h-3 ${loading || isSyncing ? 'animate-spin' : ''}`} />
      </button>
    </div>
  )
}

interface PermissionBadgeProps {
  label: string
}

/**
 * Static permission badge (no sync functionality).
 * Used for permissions like "Gmail (read)" that don't have sync status.
 */
export function PermissionBadge({ label }: PermissionBadgeProps) {
  return (
    <span className="inline-flex items-center px-2.5 py-1 rounded-md text-xs font-medium bg-white border border-gray-200 text-gray-700">
      {label}
    </span>
  )
}
