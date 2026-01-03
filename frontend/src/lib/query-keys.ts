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
import type { ReminderListParams } from '@/types/reminder'
import type { ListTimeEntriesParams } from '@/types/time-entry'

// Contact query keys
export const contactKeys = {
  all: ['contacts'] as const,
  lists: () => [...contactKeys.all, 'list'] as const,
  list: (params: ContactListParams) => [...contactKeys.lists(), params] as const,
  details: () => [...contactKeys.all, 'detail'] as const,
  detail: (id: string) => [...contactKeys.details(), id] as const,
  overdue: () => [...contactKeys.all, 'overdue'] as const,
}

// Reminder query keys
export const reminderKeys = {
  all: ['reminders'] as const,
  lists: () => [...reminderKeys.all, 'list'] as const,
  list: (params: ReminderListParams) => [...reminderKeys.lists(), params] as const,
  stats: () => [...reminderKeys.all, 'stats'] as const,
  byContact: (contactId: string) => [...reminderKeys.all, 'contact', contactId] as const,
}

// Time entry query keys
export const timeEntryKeys = {
  all: ['time-entries'] as const,
  lists: () => [...timeEntryKeys.all, 'list'] as const,
  list: (params: ListTimeEntriesParams) => [...timeEntryKeys.lists(), params] as const,
  details: () => [...timeEntryKeys.all, 'detail'] as const,
  detail: (id: string) => [...timeEntryKeys.details(), id] as const,
  running: () => [...timeEntryKeys.all, 'running'] as const,
  stats: () => [...timeEntryKeys.all, 'stats'] as const,
}

// System query keys
export const systemKeys = {
  all: ['system'] as const,
  time: () => [...systemKeys.all, 'time'] as const,
}
