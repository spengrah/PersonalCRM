/**
 * Centralized query key definitions for React Query.
 *
 * All query keys should be defined here to:
 * 1. Avoid circular dependencies between hooks
 * 2. Provide a single source of truth for cache invalidation
 * 3. Enable type-safe query key references
 *
 * @see docs/FRONTEND_STATE.md for full documentation
 */

import type { ContactListParams } from '@/types/contact'
import type { ImportCandidatesListParams } from '@/types/import'

// Contact query keys
export const contactKeys = {
  all: ['contacts'] as const,
  lists: () => [...contactKeys.all, 'list'] as const,
  list: (params: ContactListParams) => [...contactKeys.lists(), params] as const,
  details: () => [...contactKeys.all, 'detail'] as const,
  detail: (id: string) => [...contactKeys.details(), id] as const,
  overdue: () => [...contactKeys.all, 'overdue'] as const,
}

// Import candidate query keys
export const importKeys = {
  all: ['imports'] as const,
  lists: () => [...importKeys.all, 'list'] as const,
  list: (params: ImportCandidatesListParams) => [...importKeys.lists(), params] as const,
  details: () => [...importKeys.all, 'detail'] as const,
  detail: (id: string) => [...importKeys.details(), id] as const,
}

// System query keys
export const systemKeys = {
  all: ['system'] as const,
  time: () => [...systemKeys.all, 'time'] as const,
}

// Sync state query keys
export const syncKeys = {
  all: ['sync'] as const,
  states: () => [...syncKeys.all, 'states'] as const,
  state: (source: string, accountId?: string) =>
    [...syncKeys.all, 'state', source, accountId] as const,
}
