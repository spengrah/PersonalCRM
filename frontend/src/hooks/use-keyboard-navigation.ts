'use client'

import { useEffect, useMemo, useCallback } from 'react'

/**
 * Configuration for keyboard navigation hook
 */
export interface UseKeyboardNavigationOptions {
  /** Ordered list of item IDs to navigate through */
  ids: string[]
  /** Current item ID */
  currentId: string
  /** Called when user navigates to a new item */
  onNavigate: (id: string, index: number) => void
  /** Disable keyboard handling (e.g., when editing, loading) */
  enabled?: boolean
  /** Key bindings - defaults to arrow keys */
  keys?: {
    prev?: string // default: 'ArrowLeft'
    next?: string // default: 'ArrowRight'
  }
}

/**
 * Return value from keyboard navigation hook
 */
export interface UseKeyboardNavigationResult {
  /** Current position in the list (0-indexed) */
  currentIndex: number
  /** Total number of items */
  total: number
  /** Whether navigation backward is possible */
  canGoBack: boolean
  /** Whether navigation forward is possible */
  canGoForward: boolean
  /** Navigate to previous item */
  goBack: () => void
  /** Navigate to next item */
  goForward: () => void
}

/**
 * Reusable keyboard navigation hook for detail pages
 *
 * Handles:
 * - Arrow key navigation (← previous, → next)
 * - Boundary detection (disable at first/last)
 * - Input field exclusion
 * - Enable/disable toggle (for edit mode)
 *
 * @example
 * ```tsx
 * const { canGoBack, canGoForward, goBack, goForward } = useKeyboardNavigation({
 *   ids: navigationIds,
 *   currentId: contactId,
 *   onNavigate: (id) => router.push(`/contacts/${id}?${searchParams.toString()}`),
 *   enabled: !isEditing && !isLoading,
 * })
 * ```
 */
export function useKeyboardNavigation({
  ids,
  currentId,
  onNavigate,
  enabled = true,
  keys = {},
}: UseKeyboardNavigationOptions): UseKeyboardNavigationResult {
  const prevKey = keys.prev ?? 'ArrowLeft'
  const nextKey = keys.next ?? 'ArrowRight'

  // Calculate current position
  const currentIndex = useMemo(() => {
    if (!currentId || ids.length === 0) return -1
    return ids.indexOf(currentId)
  }, [ids, currentId])

  // Boundary checks
  const canGoBack = currentIndex > 0
  const canGoForward = currentIndex >= 0 && currentIndex < ids.length - 1
  const total = ids.length

  // Navigation functions
  const goBack = useCallback(() => {
    if (!enabled || !canGoBack || currentIndex < 0) return
    const newIndex = currentIndex - 1
    onNavigate(ids[newIndex], newIndex)
  }, [enabled, canGoBack, ids, currentIndex, onNavigate])

  const goForward = useCallback(() => {
    if (!enabled || !canGoForward || currentIndex < 0) return
    const newIndex = currentIndex + 1
    onNavigate(ids[newIndex], newIndex)
  }, [enabled, canGoForward, ids, currentIndex, onNavigate])

  // Keyboard event handler
  useEffect(() => {
    function handleKeyDown(event: KeyboardEvent) {
      // Don't handle if typing in an input
      if (event.target instanceof HTMLInputElement || event.target instanceof HTMLTextAreaElement) {
        return
      }

      // Ignore if navigation is disabled
      if (!enabled) return

      switch (event.key) {
        case prevKey:
          event.preventDefault()
          goBack()
          break
        case nextKey:
          event.preventDefault()
          goForward()
          break
      }
    }

    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [enabled, prevKey, nextKey, goBack, goForward])

  return {
    currentIndex,
    total,
    canGoBack,
    canGoForward,
    goBack,
    goForward,
  }
}
