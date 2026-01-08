/**
 * Types for sync state and status
 */

export type SyncStatus = 'idle' | 'syncing' | 'error' | 'disabled'

export interface SyncState {
  id: string
  source: string
  account_id: string | null
  enabled: boolean
  status: SyncStatus
  sync_cursor: string | null
  last_sync_at: string | null
  last_successful_sync_at: string | null
  next_sync_at: string | null
  error_count: number
  error_message: string | null
  created_at: string
  updated_at: string
}
