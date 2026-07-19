import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

// Must use vi.hoisted for variables used in vi.mock
const mockInvalidateQueries = vi.hoisted(() => vi.fn())
const mockGetQueryCache = vi.hoisted(() =>
  vi.fn(() => ({
    getAll: () => [],
  }))
)

vi.mock('../query-client', () => ({
  queryClient: {
    invalidateQueries: mockInvalidateQueries,
    getQueryCache: mockGetQueryCache,
  },
}))

// Import after mocking
import { invalidateFor, type DomainEvent } from '../query-invalidation'
import { contactKeys, importKeys, syncKeys } from '../query-keys'

describe('query-invalidation', () => {
  beforeEach(() => {
    mockInvalidateQueries.mockClear()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  describe('invalidateFor', () => {
    describe('contact events', () => {
      it('invalidates contact lists on contact:created', () => {
        invalidateFor('contact:created')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(1)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
      })

      it('invalidates contact lists on contact:updated', () => {
        invalidateFor('contact:updated')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(1)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
      })

      it('invalidates contact lists on contact:deleted', () => {
        invalidateFor('contact:deleted')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(1)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
      })

      it('invalidates contacts and overdue on contact:touched', () => {
        invalidateFor('contact:touched')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(2)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.overdue(),
        })
      })

      it('invalidates contacts and overdue on contact:merged', () => {
        invalidateFor('contact:merged')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(2)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.overdue(),
        })
      })
    })

    describe('import events', () => {
      it('invalidates import, suggestions, and contact lists on import:imported', () => {
        invalidateFor('import:imported')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(3)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
        // The suggestions surface composes the same candidate list.
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.suggestionsLists(),
        })
        // Cross-domain: importing creates a new contact
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
      })

      // Turns red if the (contactId) => contactKeys.detail(contactId) entry is
      // dropped from the import:linked rule: a linked contact's cached detail
      // view would keep serving its pre-link method list.
      it('invalidates the linked contact detail key on import:linked', () => {
        invalidateFor('import:linked', 'crm-456')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(4)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.suggestionsLists(),
        })
        // Cross-domain: linking enriches an existing contact
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.detail('crm-456'),
        })
      })

      // The factory-skip contract: invalidateFor drops factory entries when no
      // contactId is supplied, so the static keys must still fire for callers
      // that have no contact in hand.
      it('invalidates only the static keys on import:linked without a contact id', () => {
        invalidateFor('import:linked')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(3)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.suggestionsLists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
      })

      it('invalidates import and suggestions lists on import:ignored', () => {
        invalidateFor('import:ignored')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(2)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.suggestionsLists(),
        })
      })

      it('invalidates import lists, suggestions, and sync states on import:synced', () => {
        invalidateFor('import:synced')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(3)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.suggestionsLists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: syncKeys.states(),
        })
      })

      it('invalidates suggestions, import, and contact on method-suggestion:resolved', () => {
        invalidateFor('method-suggestion:resolved', 'contact-1')

        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.suggestionsLists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.detail('contact-1'),
        })
      })

      it('invalidates only suggestions on method-suggestion:dismissed', () => {
        invalidateFor('method-suggestion:dismissed')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(1)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.suggestionsLists(),
        })
      })
    })
  })

  describe('type safety', () => {
    it('accepts all valid domain events', () => {
      const validEvents: DomainEvent[] = [
        'contact:created',
        'contact:updated',
        'contact:deleted',
        'contact:touched',
        'contact:merged',
        'import:imported',
        'import:linked',
        'import:ignored',
        'import:synced',
        'method-suggestion:resolved',
        'method-suggestion:dismissed',
      ]

      // This test verifies the type definitions are correct
      // If any event is missing from the type, TypeScript will catch it
      validEvents.forEach(event => {
        mockInvalidateQueries.mockClear()
        expect(() => invalidateFor(event)).not.toThrow()
        expect(mockInvalidateQueries).toHaveBeenCalled()
      })
    })
  })
})
