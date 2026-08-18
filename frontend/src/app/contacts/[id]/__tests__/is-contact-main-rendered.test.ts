import { describe, expect, it } from 'vitest'
import { isContactMainRendered } from '../page'

describe('isContactMainRendered', () => {
  it('is false while loading', () => {
    expect(isContactMainRendered({ isLoading: true, error: null, contact: { id: 'a' } })).toBe(
      false
    )
  })

  it('is false with no contact and no error (should not happen, but must not crash)', () => {
    expect(isContactMainRendered({ isLoading: false, error: null, contact: null })).toBe(false)
  })

  it('is false when a background refetch errors but a stale contact is still cached', () => {
    // The race this predicate exists for: TanStack Query keeps `data` from the
    // last successful fetch while a later refetch is in flight, so `contact`
    // can be truthy at the same instant `error` becomes truthy too. The page
    // takes its not-found return in this state — nothing is mounted.
    expect(
      isContactMainRendered({
        isLoading: false,
        error: new Error('refetch failed'),
        contact: { id: 'a' },
      })
    ).toBe(false)
  })

  it('is true once loaded cleanly', () => {
    expect(isContactMainRendered({ isLoading: false, error: null, contact: { id: 'a' } })).toBe(
      true
    )
  })
})
