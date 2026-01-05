/**
 * Centralized query invalidation registry.
 *
 * This module defines the mapping between domain events and the query keys
 * that should be invalidated when those events occur. This ensures that
 * cross-domain side effects (e.g., marking a contact as contacted also
 * completes auto-reminders) are properly reflected in the UI.
 *
 * @see docs/FRONTEND_STATE.md for full documentation
 */

import { queryClient } from './query-client'
import { contactKeys, importKeys, reminderKeys, timeEntryKeys } from './query-keys'

/**
 * Domain events that trigger query invalidations.
 *
 * Each event corresponds to a mutation that may affect cached data.
 * The naming convention is `domain:action` (e.g., `contact:created`).
 */
export type DomainEvent =
  // Contact events
  | 'contact:created'
  | 'contact:updated'
  | 'contact:deleted'
  | 'contact:touched' // marked as contacted
  // Import events
  | 'import:imported' // imported as new contact
  | 'import:linked' // linked to existing contact
  | 'import:ignored' // marked as ignored
  | 'import:synced' // sync completed
  // Reminder events
  | 'reminder:created'
  | 'reminder:completed'
  | 'reminder:deleted'
  // Time entry events
  | 'time-entry:created'
  | 'time-entry:updated'
  | 'time-entry:deleted'

/**
 * Invalidation rules mapping domain events to affected query keys.
 *
 * When a mutation fires a domain event, all query keys listed for that
 * event will be invalidated, triggering a refetch if the query is active.
 *
 * IMPORTANT: Keep this in sync with backend behavior. If the backend
 * has side effects that modify other domains, those domains must be
 * included in the invalidation rules.
 */
const invalidationRules: Record<DomainEvent, readonly unknown[][]> = {
  // Contact events
  'contact:created': [contactKeys.lists()],
  'contact:updated': [contactKeys.lists()],
  // Backend soft-deletes reminders when contact is deleted
  'contact:deleted': [contactKeys.lists(), reminderKeys.all],
  // Backend completes auto-reminders when contact is marked as contacted
  'contact:touched': [contactKeys.lists(), contactKeys.overdue(), reminderKeys.all],

  // Import events
  // Importing creates a new contact, so invalidate both imports and contacts
  'import:imported': [importKeys.lists(), contactKeys.lists()],
  // Linking enriches an existing contact
  'import:linked': [importKeys.lists(), contactKeys.lists()],
  // Ignoring only affects the imports list
  'import:ignored': [importKeys.lists()],
  // Sync may add new candidates
  'import:synced': [importKeys.lists()],

  // Reminder events (all invalidate the entire reminders domain)
  'reminder:created': [reminderKeys.all],
  'reminder:completed': [reminderKeys.all],
  'reminder:deleted': [reminderKeys.all],

  // Time entry events
  'time-entry:created': [timeEntryKeys.lists(), timeEntryKeys.running(), timeEntryKeys.stats()],
  'time-entry:updated': [timeEntryKeys.lists(), timeEntryKeys.running(), timeEntryKeys.stats()],
  'time-entry:deleted': [timeEntryKeys.lists(), timeEntryKeys.running(), timeEntryKeys.stats()],
}

/**
 * Invalidate all queries affected by a domain event.
 *
 * This is the single source of truth for cross-domain cache invalidation.
 * Use this instead of calling `queryClient.invalidateQueries()` directly
 * in mutation handlers.
 *
 * @example
 * ```typescript
 * onSuccess: (updatedContact) => {
 *   queryClient.setQueryData(contactKeys.detail(updatedContact.id), updatedContact)
 *   invalidateFor('contact:touched')
 * }
 * ```
 */
export function invalidateFor(event: DomainEvent): void {
  const keys = invalidationRules[event]
  keys.forEach(queryKey => {
    queryClient.invalidateQueries({ queryKey })
  })
}

// Re-export keys for convenience (avoids needing two imports)
export { contactKeys, importKeys, reminderKeys, timeEntryKeys, systemKeys } from './query-keys'
