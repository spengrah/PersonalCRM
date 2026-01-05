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
import { contactKeys, importKeys, reminderKeys, timeEntryKeys } from '../query-keys'

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

      it('invalidates contacts and reminders on contact:deleted', () => {
        invalidateFor('contact:deleted')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(2)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
        // Cross-domain: deleting contact also affects reminders
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: reminderKeys.all,
        })
      })

      it('invalidates contacts, overdue, and reminders on contact:touched', () => {
        invalidateFor('contact:touched')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(3)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: contactKeys.overdue(),
        })
        // Cross-domain: marking as contacted completes auto-reminders
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: reminderKeys.all,
        })
      })
    })

    describe('reminder events', () => {
      it('invalidates all reminders on reminder:created', () => {
        invalidateFor('reminder:created')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(1)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: reminderKeys.all,
        })
      })

      it('invalidates all reminders on reminder:completed', () => {
        invalidateFor('reminder:completed')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(1)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: reminderKeys.all,
        })
      })

      it('invalidates all reminders on reminder:deleted', () => {
        invalidateFor('reminder:deleted')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(1)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: reminderKeys.all,
        })
      })
    })

    describe('time entry events', () => {
      it('invalidates time entry queries on time-entry:created', () => {
        invalidateFor('time-entry:created')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(3)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: timeEntryKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: timeEntryKeys.running(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: timeEntryKeys.stats(),
        })
      })

      it('invalidates time entry queries on time-entry:updated', () => {
        invalidateFor('time-entry:updated')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(3)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: timeEntryKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: timeEntryKeys.running(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: timeEntryKeys.stats(),
        })
      })

      it('invalidates time entry queries on time-entry:deleted', () => {
        invalidateFor('time-entry:deleted')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(3)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: timeEntryKeys.lists(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: timeEntryKeys.running(),
        })
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: timeEntryKeys.stats(),
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

      it('invalidates import lists on import:synced', () => {
        invalidateFor('import:synced')

        expect(mockInvalidateQueries).toHaveBeenCalledTimes(1)
        expect(mockInvalidateQueries).toHaveBeenCalledWith({
          queryKey: importKeys.lists(),
        })
      })
    })

    describe('cross-domain invalidation', () => {
      it('contact:touched invalidates reminder queries (backend completes auto-reminders)', () => {
        invalidateFor('contact:touched')

        const calls = mockInvalidateQueries.mock.calls.map(call => call[0].queryKey)
        expect(calls).toContainEqual(reminderKeys.all)
      })

      it('contact:deleted invalidates reminder queries (cascade delete)', () => {
        invalidateFor('contact:deleted')

        const calls = mockInvalidateQueries.mock.calls.map(call => call[0].queryKey)
        expect(calls).toContainEqual(reminderKeys.all)
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
        'import:imported',
        'import:linked',
        'import:ignored',
        'import:synced',
        'reminder:created',
        'reminder:completed',
        'reminder:deleted',
        'time-entry:created',
        'time-entry:updated',
        'time-entry:deleted',
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
