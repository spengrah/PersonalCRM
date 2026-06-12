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

// Breach type emitted by the sync-staleness watchdog (#480).
export type StalenessBreachType = 'heartbeat' | 'sync_stale' | 'push_stale' | 'sync_error'

// StalenessBreach mirrors the backend repository.StalenessBreach DTO returned
// by GET /api/v1/sync/staleness. The read path returns active (unresolved)
// breaches only, so resolved_at is always absent there.
export interface StalenessBreach {
  id: string
  source: string
  account_id: string
  breach_type: StalenessBreachType
  // ISO timestamp of the stale reference (last heartbeat / success / push).
  stale_since: string
  threshold_seconds: number
  details: string
  detected_at: string
  last_observed_at: string
}
