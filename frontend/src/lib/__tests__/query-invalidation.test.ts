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
      it('invalidates import and contact lists on import:imported', () => {
        invalidateFor('import:imported')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(2)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
        // Cross-domain: importing creates a new contact
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
      })

      it('invalidates import and contact lists on import:linked', () => {
        invalidateFor('import:linked')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(2)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
        // Cross-domain: linking enriches an existing contact
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
      })

      it('invalidates only import lists on import:ignored', () => {
        invalidateFor('import:ignored')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(1)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
      })

      it('invalidates import lists and sync states on import:synced', () => {
        invalidateFor('import:synced')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(2)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: syncKeys.states(),
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
