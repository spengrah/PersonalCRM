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
import { contactKeys, importKeys, syncKeys, contactTaskKeys } from './query-keys'

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
  | 'contact:touched' // marked as contacted (legacy alias kept during PR-B for listener compatibility)
  | 'contact:merged' // merged with another contact
  // Interaction events
  | 'interaction:created' // a manual / system interaction was logged
  // Import events
  | 'import:imported' // imported as new contact
  | 'import:linked' // linked to existing contact
  | 'import:ignored' // marked as ignored
  | 'import:synced' // sync completed
  // Contact task events
  | 'task:created' // action task created
  | 'task:deleted' // task link removed

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
// InvalidationKey is either a static query key array or a factory that
// builds a per-contact key from the contactId argument supplied to
// invalidateFor. The factory shape lets us invalidate contactKeys.detail(id)
// + contactTaskKeys.list(id) on per-contact mutations like
// interaction:created without burning the whole list cache for every
// other contact.
type InvalidationKey = readonly unknown[] | ((contactId: string) => readonly unknown[])

const invalidationRules: Record<DomainEvent, InvalidationKey[]> = {
  // Contact events
  'contact:created': [contactKeys.lists()],
  'contact:updated': [contactKeys.lists()],
  'contact:deleted': [contactKeys.lists()],
  'contact:touched': [contactKeys.lists(), contactKeys.overdue()],
  'contact:merged': [contactKeys.lists(), contactKeys.overdue()],

  // Interaction events — a manual interaction may bump cadence columns,
  // auto-complete a pending follow-up, or shift the overdue queue.
  // Use contactTaskKeys.forContact (3-element prefix) so React Query
  // matches every filtered variant for the contact; passing
  // contactTaskKeys.list(contactId) here would push a 4th `undefined`
  // slot that React Query treats as a literal mismatch against keys
  // carrying real filter params.
  'interaction:created': [
    contactKeys.lists(),
    contactKeys.overdue(),
    (contactId: string) => contactKeys.detail(contactId),
    (contactId: string) => contactTaskKeys.forContact(contactId),
  ],

  // Import events
  // Importing creates a new contact, so invalidate both imports and contacts
  'import:imported': [importKeys.lists(), contactKeys.lists()],
  // Linking enriches an existing contact
  'import:linked': [importKeys.lists(), contactKeys.lists()],
  // Ignoring only affects the imports list
  'import:ignored': [importKeys.lists()],
  // Sync trigger updates sync states; completion may add new candidates
  'import:synced': [importKeys.lists(), syncKeys.states()],

  // Contact task events
  'task:created': [contactTaskKeys.lists()],
  'task:deleted': [contactTaskKeys.lists()],
}

/**
 * Invalidate all queries affected by a domain event.
 *
 * This is the single source of truth for cross-domain cache invalidation.
 * Use this instead of calling `queryClient.invalidateQueries()` directly
 * in mutation handlers.
 *
 * Some rules require a contactId (e.g., `interaction:created` invalidates
 * contactKeys.detail(contactId)). Static-key rules ignore the second arg.
 *
 * @example
 * ```typescript
 * onSuccess: (updatedContact) => {
 *   queryClient.setQueryData(contactKeys.detail(updatedContact.id), updatedContact)
 *   invalidateFor('contact:touched')
 * }
 *
 * onSuccess: (_resp, vars) => {
 *   invalidateFor('interaction:created', vars.contactId)
 * }
 * ```
 */
export function invalidateFor(event: DomainEvent, contactId?: string): void {
  const keys = invalidationRules[event]
  keys.forEach(entry => {
    const queryKey = typeof entry === 'function' ? (contactId ? entry(contactId) : null) : entry
    if (queryKey) {
      queryClient.invalidateQueries({ queryKey: queryKey as readonly unknown[] })
    }
  })
}

// Re-export keys for convenience (avoids needing two imports)
export { contactKeys, importKeys, systemKeys, syncKeys, contactTaskKeys } from './query-keys'
