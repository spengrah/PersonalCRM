/**
 * Centralized query invalidation registry.
 *
 * This module defines the mapping between domain events and the query keys
 * that should be invalidated when those events occur. This ensures that
 * cross-domain side effects (e.g., merging contacts updates overdue lists)
 * are properly reflected in the UI.
 *
 * @see docs/FRONTEND_STATE.md for full documentation
 */

import { queryClient } from './query-client'
import { contactKeys, importKeys, syncKeys } from './query-keys'

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
  | 'contact:merged' // merged with another contact
  // Import events
  | 'import:imported' // imported as new contact
  | 'import:linked' // linked to existing contact
  | 'import:ignored' // marked as ignored
  | 'import:synced' // sync completed

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
  'contact:deleted': [contactKeys.lists()],
  'contact:touched': [contactKeys.lists(), contactKeys.overdue()],
  'contact:merged': [contactKeys.lists(), contactKeys.overdue()],

  // Import events
  // Importing creates a new contact, so invalidate both imports and contacts
  'import:imported': [importKeys.lists(), contactKeys.lists()],
  // Linking enriches an existing contact
  'import:linked': [importKeys.lists(), contactKeys.lists()],
  // Ignoring only affects the imports list
  'import:ignored': [importKeys.lists()],
  // Sync trigger updates sync states; completion may add new candidates
  'import:synced': [importKeys.lists(), syncKeys.states()],
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
export { contactKeys, importKeys, systemKeys, syncKeys } from './query-keys'
