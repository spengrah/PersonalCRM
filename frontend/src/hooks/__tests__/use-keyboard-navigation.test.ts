import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useKeyboardNavigation } from '../use-keyboard-navigation'

describe('useKeyboardNavigation', () => {
  const mockOnNavigate = vi.fn()

  beforeEach(() => {
    vi.clearAllMocks()
  })

  afterEach(() => {
    vi.clearAllMocks()
  })

  describe('boundary detection', () => {
    it('should detect first position correctly', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'a',
          onNavigate: mockOnNavigate,
        })
      )

      expect(result.current.currentIndex).toBe(0)
      expect(result.current.canGoBack).toBe(false)
      expect(result.current.canGoForward).toBe(true)
      expect(result.current.total).toBe(3)
    })

    it('should detect last position correctly', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'c',
          onNavigate: mockOnNavigate,
        })
      )

      expect(result.current.currentIndex).toBe(2)
      expect(result.current.canGoBack).toBe(true)
      expect(result.current.canGoForward).toBe(false)
    })

    it('should detect middle position correctly', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
        })
      )

      expect(result.current.currentIndex).toBe(1)
      expect(result.current.canGoBack).toBe(true)
      expect(result.current.canGoForward).toBe(true)
    })

    it('should handle currentId not in list', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'unknown',
          onNavigate: mockOnNavigate,
        })
      )

      expect(result.current.currentIndex).toBe(-1)
      expect(result.current.canGoBack).toBe(false)
      expect(result.current.canGoForward).toBe(false)
    })

    it('should handle empty ids array', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: [],
          currentId: 'a',
          onNavigate: mockOnNavigate,
        })
      )

      expect(result.current.currentIndex).toBe(-1)
      expect(result.current.canGoBack).toBe(false)
      expect(result.current.canGoForward).toBe(false)
      expect(result.current.total).toBe(0)
    })
  })

  describe('navigation functions', () => {
    it('goBack should call onNavigate with previous item', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
        })
      )

      act(() => {
        result.current.goBack()
      })

      expect(mockOnNavigate).toHaveBeenCalledWith('a', 0)
    })

    it('goForward should call onNavigate with next item', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
        })
      )

      act(() => {
        result.current.goForward()
      })

      expect(mockOnNavigate).toHaveBeenCalledWith('c', 2)
    })

    it('goBack should not navigate at first position', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'a',
          onNavigate: mockOnNavigate,
        })
      )

      act(() => {
        result.current.goBack()
      })

      expect(mockOnNavigate).not.toHaveBeenCalled()
    })

    it('goForward should not navigate at last position', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'c',
          onNavigate: mockOnNavigate,
        })
      )

      act(() => {
        result.current.goForward()
      })

      expect(mockOnNavigate).not.toHaveBeenCalled()
    })

    it('goBack should not navigate when currentId not in list', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'unknown',
          onNavigate: mockOnNavigate,
        })
      )

      act(() => {
        result.current.goBack()
      })

      expect(mockOnNavigate).not.toHaveBeenCalled()
    })

    it('goForward should not navigate when currentId not in list', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'unknown',
          onNavigate: mockOnNavigate,
        })
      )

      act(() => {
        result.current.goForward()
      })

      expect(mockOnNavigate).not.toHaveBeenCalled()
    })
  })

  describe('enabled toggle', () => {
    it('should not navigate when disabled', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
          enabled: false,
        })
      )

      act(() => {
        result.current.goBack()
        result.current.goForward()
      })

      expect(mockOnNavigate).not.toHaveBeenCalled()
    })

    it('should navigate when enabled', () => {
      const { result } = renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
          enabled: true,
        })
      )

      act(() => {
        result.current.goForward()
      })

      expect(mockOnNavigate).toHaveBeenCalledWith('c', 2)
    })
  })

  describe('keyboard events', () => {
    it('should navigate on ArrowLeft key', () => {
      renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
        })
      )

      act(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft' }))
      })

      expect(mockOnNavigate).toHaveBeenCalledWith('a', 0)
    })

    it('should navigate on ArrowRight key', () => {
      renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
        })
      )

      act(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight' }))
      })

      expect(mockOnNavigate).toHaveBeenCalledWith('c', 2)
    })

    it('should not navigate on arrow keys when disabled', () => {
      renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
          enabled: false,
        })
      )

      act(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft' }))
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight' }))
      })

      expect(mockOnNavigate).not.toHaveBeenCalled()
    })

    it('should not navigate when target is input element', () => {
      renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
        })
      )

      // Create an input element and dispatch event from it
      const input = document.createElement('input')
      document.body.appendChild(input)
      input.focus()

      act(() => {
        const event = new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true })
        Object.defineProperty(event, 'target', { value: input })
        window.dispatchEvent(event)
      })

      expect(mockOnNavigate).not.toHaveBeenCalled()
      document.body.removeChild(input)
    })

    it('should not navigate when target is textarea element', () => {
      renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
        })
      )

      // Create a textarea element and dispatch event from it
      const textarea = document.createElement('textarea')
      document.body.appendChild(textarea)
      textarea.focus()

      act(() => {
        const event = new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true })
        Object.defineProperty(event, 'target', { value: textarea })
        window.dispatchEvent(event)
      })

      expect(mockOnNavigate).not.toHaveBeenCalled()
      document.body.removeChild(textarea)
    })

    it('should use custom key bindings', () => {
      renderHook(() =>
        useKeyboardNavigation({
          ids: ['a', 'b', 'c'],
          currentId: 'b',
          onNavigate: mockOnNavigate,
          keys: { prev: 'j', next: 'k' },
        })
      )

      // Default arrow keys should not work
      act(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft' }))
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight' }))
      })
      expect(mockOnNavigate).not.toHaveBeenCalled()

      // Custom keys should work
      act(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'j' }))
      })
      expect(mockOnNavigate).toHaveBeenCalledWith('a', 0)

      mockOnNavigate.mockClear()

      act(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k' }))
      })
      expect(mockOnNavigate).toHaveBeenCalledWith('c', 2)
    })
  })

  describe('reactivity', () => {
    it('should update when ids change', () => {
      const { result, rerender } = renderHook(
        ({ ids, currentId }) =>
          useKeyboardNavigation({
            ids,
            currentId,
            onNavigate: mockOnNavigate,
          }),
        { initialProps: { ids: ['a', 'b', 'c'], currentId: 'b' } }
      )

      expect(result.current.total).toBe(3)
      expect(result.current.currentIndex).toBe(1)

      // Change ids
      rerender({ ids: ['a', 'b', 'c', 'd', 'e'], currentId: 'b' })

      expect(result.current.total).toBe(5)
      expect(result.current.currentIndex).toBe(1)
    })

    it('should update when currentId changes', () => {
      const { result, rerender } = renderHook(
        ({ ids, currentId }) =>
          useKeyboardNavigation({
            ids,
            currentId,
            onNavigate: mockOnNavigate,
          }),
        { initialProps: { ids: ['a', 'b', 'c'], currentId: 'a' } }
      )

      expect(result.current.currentIndex).toBe(0)
      expect(result.current.canGoBack).toBe(false)

      // Change currentId
      rerender({ ids: ['a', 'b', 'c'], currentId: 'c' })

      expect(result.current.currentIndex).toBe(2)
      expect(result.current.canGoForward).toBe(false)
    })
  })
})
