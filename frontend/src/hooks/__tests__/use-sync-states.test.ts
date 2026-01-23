import { describe, it, expect } from 'vitest'
import {
  getAggregateSyncStatus,
  getSyncStateForAccount,
  getSyncIconClasses,
  formatSyncTime,
} from '../use-sync-states'
import type { SyncState } from '@/types/sync'

// Helper to create a mock sync state
function createSyncState(overrides: Partial<SyncState> = {}): SyncState {
  return {
    id: 'test-id',
    source: 'google',
    account_id: 'account-1',
    enabled: true,
    status: 'idle',
    sync_cursor: null,
    last_sync_at: null,
    last_successful_sync_at: null,
    next_sync_at: null,
    error_count: 0,
    error_message: null,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

describe('getAggregateSyncStatus', () => {
  describe('empty/undefined states', () => {
    it('should return synced for undefined states', () => {
      expect(getAggregateSyncStatus(undefined)).toBe('synced')
    })

    it('should return synced for empty array', () => {
      expect(getAggregateSyncStatus([])).toBe('synced')
    })
  })

  describe('single state', () => {
    it('should return synced for idle status', () => {
      const states = [createSyncState({ status: 'idle' })]
      expect(getAggregateSyncStatus(states)).toBe('synced')
    })

    it('should return syncing for syncing status', () => {
      const states = [createSyncState({ status: 'syncing' })]
      expect(getAggregateSyncStatus(states)).toBe('syncing')
    })

    it('should return error for error status', () => {
      const states = [createSyncState({ status: 'error' })]
      expect(getAggregateSyncStatus(states)).toBe('error')
    })

    it('should return synced for disabled status', () => {
      const states = [createSyncState({ status: 'disabled' })]
      expect(getAggregateSyncStatus(states)).toBe('synced')
    })
  })

  describe('multiple states with priority', () => {
    it('should return syncing when any state is syncing (syncing takes priority)', () => {
      const states = [
        createSyncState({ id: '1', status: 'idle' }),
        createSyncState({ id: '2', status: 'syncing' }),
        createSyncState({ id: '3', status: 'error' }),
      ]
      expect(getAggregateSyncStatus(states)).toBe('syncing')
    })

    it('should return error when no syncing but has error', () => {
      const states = [
        createSyncState({ id: '1', status: 'idle' }),
        createSyncState({ id: '2', status: 'error' }),
        createSyncState({ id: '3', status: 'disabled' }),
      ]
      expect(getAggregateSyncStatus(states)).toBe('error')
    })

    it('should return synced when all states are idle or disabled', () => {
      const states = [
        createSyncState({ id: '1', status: 'idle' }),
        createSyncState({ id: '2', status: 'disabled' }),
        createSyncState({ id: '3', status: 'idle' }),
      ]
      expect(getAggregateSyncStatus(states)).toBe('synced')
    })

    it('should return syncing even with multiple errors if any is syncing', () => {
      const states = [
        createSyncState({ id: '1', status: 'error' }),
        createSyncState({ id: '2', status: 'error' }),
        createSyncState({ id: '3', status: 'syncing' }),
      ]
      expect(getAggregateSyncStatus(states)).toBe('syncing')
    })
  })
})

describe('getSyncIconClasses', () => {
  it('should return green pulse classes for syncing status', () => {
    expect(getSyncIconClasses('syncing')).toBe('text-green-600 animate-sync-pulse')
  })

  it('should return red class for error status', () => {
    expect(getSyncIconClasses('error')).toBe('text-red-700')
  })

  it('should return empty string for synced status', () => {
    expect(getSyncIconClasses('synced')).toBe('')
  })
})

describe('getSyncStateForAccount', () => {
  it('should return undefined for undefined states', () => {
    expect(getSyncStateForAccount(undefined, 'google', 'account-1')).toBeUndefined()
  })

  it('should return undefined for empty states', () => {
    expect(getSyncStateForAccount([], 'google', 'account-1')).toBeUndefined()
  })

  it('should find matching state by source and account_id', () => {
    const states = [
      createSyncState({ id: '1', source: 'google', account_id: 'account-1' }),
      createSyncState({ id: '2', source: 'google', account_id: 'account-2' }),
      createSyncState({ id: '3', source: 'outlook', account_id: 'account-1' }),
    ]
    const result = getSyncStateForAccount(states, 'google', 'account-2')
    expect(result?.id).toBe('2')
  })

  it('should return undefined if no match found', () => {
    const states = [createSyncState({ id: '1', source: 'google', account_id: 'account-1' })]
    expect(getSyncStateForAccount(states, 'outlook', 'account-1')).toBeUndefined()
    expect(getSyncStateForAccount(states, 'google', 'account-999')).toBeUndefined()
  })
})

describe('formatSyncTime', () => {
  it('should return "Never" for null input', () => {
    expect(formatSyncTime(null)).toBe('Never')
  })

  it('should return "Just now" for times less than 1 minute ago', () => {
    const now = new Date()
    const thirtySecondsAgo = new Date(now.getTime() - 30 * 1000)
    expect(formatSyncTime(thirtySecondsAgo.toISOString())).toBe('Just now')
  })

  it('should return minutes ago for times less than 1 hour ago', () => {
    const now = new Date()
    const fifteenMinutesAgo = new Date(now.getTime() - 15 * 60 * 1000)
    expect(formatSyncTime(fifteenMinutesAgo.toISOString())).toBe('15m ago')
  })

  it('should return hours ago for times less than 24 hours ago', () => {
    const now = new Date()
    const threeHoursAgo = new Date(now.getTime() - 3 * 60 * 60 * 1000)
    expect(formatSyncTime(threeHoursAgo.toISOString())).toBe('3h ago')
  })

  it('should return days ago for times less than 7 days ago', () => {
    const now = new Date()
    const twoDaysAgo = new Date(now.getTime() - 2 * 24 * 60 * 60 * 1000)
    expect(formatSyncTime(twoDaysAgo.toISOString())).toBe('2d ago')
  })

  it('should return formatted date for times 7+ days ago', () => {
    const now = new Date()
    const tenDaysAgo = new Date(now.getTime() - 10 * 24 * 60 * 60 * 1000)
    const result = formatSyncTime(tenDaysAgo.toISOString())
    // Should be a month/day format like "Jan 12"
    expect(result).toMatch(/^[A-Z][a-z]{2} \d{1,2}$/)
  })
})
