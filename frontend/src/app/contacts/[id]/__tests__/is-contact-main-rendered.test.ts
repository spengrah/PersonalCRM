import { describe, expect, it } from 'vitest'
import { isAnyDetailModalOpen, isContactMainRendered } from '../modal-gate'

describe('isContactMainRendered', () => {
  it('is false while loading', () => {
    expect(
      isContactMainRendered({
        isLoading: true,
        error: null,
        contact: { id: 'a' },
        isEditing: false,
      })
    ).toBe(false)
  })

  it('is false with no contact and no error (should not happen, but must not crash)', () => {
    expect(
      isContactMainRendered({ isLoading: false, error: null, contact: null, isEditing: false })
    ).toBe(false)
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
        isEditing: false,
      })
    ).toBe(false)
  })

  it('is false while editing, even with a loaded contact — a modal flag that leaked into edit mode has nothing left to dismiss it', () => {
    // A modal that doesn't trap focus (e.g. MergeContactModal) can leave Tab
    // able to reach the underlying Edit button; activating it flips isEditing
    // via a native click, bypassing this page's own keydown handler entirely.
    // The edit-mode early return then unmounts the modal, but its own open
    // flag survives — so the mounted-modal predicate must exclude edit mode
    // too, or a stale flag would swallow Escape in the edit view.
    expect(
      isContactMainRendered({
        isLoading: false,
        error: null,
        contact: { id: 'a' },
        isEditing: true,
      })
    ).toBe(false)
  })

  it('is true once loaded cleanly, outside edit mode', () => {
    expect(
      isContactMainRendered({
        isLoading: false,
        error: null,
        contact: { id: 'a' },
        isEditing: false,
      })
    ).toBe(true)
  })
})

describe('isAnyDetailModalOpen', () => {
  it('is false when main is not rendered, even with a modal flag set — the regression this composition exists to catch', () => {
    // A shape like `(isMainRendered && (merge || log)) || addTask` would let
    // addTask bypass the gate on its own; a stale contact from a failed
    // background refetch could leave isAddTaskModalOpen=true with nothing
    // mounted to dismiss it, swallowing Escape on the not-found view.
    expect(
      isAnyDetailModalOpen({
        isMainRendered: false,
        isMergeModalOpen: false,
        isLogModalOpen: false,
        isAddTaskModalOpen: true,
      })
    ).toBe(false)
    expect(
      isAnyDetailModalOpen({
        isMainRendered: false,
        isMergeModalOpen: true,
        isLogModalOpen: false,
        isAddTaskModalOpen: false,
      })
    ).toBe(false)
    expect(
      isAnyDetailModalOpen({
        isMainRendered: false,
        isMergeModalOpen: false,
        isLogModalOpen: true,
        isAddTaskModalOpen: false,
      })
    ).toBe(false)
  })

  it('is true for each modal once main is rendered', () => {
    expect(
      isAnyDetailModalOpen({
        isMainRendered: true,
        isMergeModalOpen: true,
        isLogModalOpen: false,
        isAddTaskModalOpen: false,
      })
    ).toBe(true)
    expect(
      isAnyDetailModalOpen({
        isMainRendered: true,
        isMergeModalOpen: false,
        isLogModalOpen: true,
        isAddTaskModalOpen: false,
      })
    ).toBe(true)
    expect(
      isAnyDetailModalOpen({
        isMainRendered: true,
        isMergeModalOpen: false,
        isLogModalOpen: false,
        isAddTaskModalOpen: true,
      })
    ).toBe(true)
  })

  it('is false when nothing is open, even with main rendered', () => {
    expect(
      isAnyDetailModalOpen({
        isMainRendered: true,
        isMergeModalOpen: false,
        isLogModalOpen: false,
        isAddTaskModalOpen: false,
      })
    ).toBe(false)
  })
})
