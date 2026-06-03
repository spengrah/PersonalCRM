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
  | 'contact:touched' // marked as contacted (legacy alias preserved for existing listener compatibility)
  | 'contact:merged' // merged with another contact
  // Interaction events
  | 'interaction:created' // a manual / system interaction was logged
  // Import events
  | 'import:imported' // imported as new contact
  | 'import:linked' // linked to existing contact
  | 'import:ignored' // marked as ignored
  | 'import:synced' // sync completed
  // Method-suggestion events (unified suggestions surface)
  | 'method-suggestion:resolved' // pending methods confirmed onto a contact
  | 'method-suggestion:dismissed' // pending methods dismissed
  // Anarlog interactions-queue events
  | 'meeting-note:resolved' // a conflict/orphan was resolved (This one / None of these / impromptu)
  | 'name-candidate:resolved' // an anarlog_title token group was imported/linked/ignored
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
  // Importing creates a new contact, so invalidate both imports and contacts.
  // The suggestions surface composes the same candidate list, so it must
  // refresh too.
  'import:imported': [importKeys.lists(), importKeys.suggestionsLists(), contactKeys.lists()],
  // Linking enriches an existing contact
  'import:linked': [importKeys.lists(), importKeys.suggestionsLists(), contactKeys.lists()],
  // Ignoring only affects the imports + suggestions lists
  'import:ignored': [importKeys.lists(), importKeys.suggestionsLists()],
  // Sync trigger updates sync states; completion may add new candidates
  'import:synced': [importKeys.lists(), importKeys.suggestionsLists(), syncKeys.states()],

  // Resolving method suggestions enriches an already-linked contact, so the
  // suggestions surface + contact lists/detail refresh. The affected
  // contact's detail key is invalidated via the factory pattern.
  'method-suggestion:resolved': [
    importKeys.suggestionsLists(),
    importKeys.lists(),
    contactKeys.lists(),
    (contactId: string) => contactKeys.detail(contactId),
  ],
  // Dismissing only mutates the suggestions surface (no contact change).
  'method-suggestion:dismissed': [importKeys.suggestionsLists()],

  // Resolving a conflict/orphan always removes the card from the
  // Interactions queue + decrements the badge, even when zero walk-in
  // interactions were created. The static keys fire unconditionally; the
  // per-contact keys are invalidated per affected contact when
  // interactions_created is non-empty (the caller loops with a contactId).
  'meeting-note:resolved': [
    importKeys.needsAttention(),
    contactKeys.lists(),
    contactKeys.overdue(),
    (contactId: string) => contactKeys.detail(contactId),
    (contactId: string) => contactTaskKeys.forContact(contactId),
  ],

  // Resolving an anarlog_title token group removes it from the name-candidate
  // list; import creates a contact and link mutates cadence/contact_by, so
  // the contact lists + overdue queue may shift. The affected contact's
  // detail key is invalidated when a contactId is supplied (import: the
  // created id; link: crm_contact_id).
  'name-candidate:resolved': [
    importKeys.anarlogTitle(),
    importKeys.lists(),
    contactKeys.lists(),
    contactKeys.overdue(),
    (contactId: string) => contactKeys.detail(contactId),
  ],

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
