'use client'

import { useSyncExternalStore } from 'react'
import { clsx } from 'clsx'

/**
 * The Review setting: one value the browser holds, read by the Threads list
 * and every thread page, so changing it on either changes it on both and it
 * stays as set across visits. No server setting holds it.
 */
const STORAGE_KEY = 'crm:review'
const listeners = new Set<() => void>()
let current: boolean | undefined

function stored(): boolean {
  try {
    return window.localStorage.getItem(STORAGE_KEY) === 'on'
  } catch {
    return false
  }
}

function subscribe(onChange: () => void) {
  listeners.add(onChange)
  return () => {
    listeners.delete(onChange)
  }
}

function snapshot(): boolean {
  current ??= stored()
  return current
}

function set(on: boolean) {
  current = on
  try {
    window.localStorage.setItem(STORAGE_KEY, on ? 'on' : 'off')
  } catch {
    // The setting then lasts until the page closes.
  }
  listeners.forEach(listener => listener())
}

/** Whether Review is on, and the setter that changes it for every page. */
export function useReview(): [boolean, (on: boolean) => void] {
  return [useSyncExternalStore(subscribe, snapshot, () => false), set]
}

export function ReviewSwitch() {
  const [on, setOn] = useReview()
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      onClick={() => setOn(!on)}
      className="inline-flex items-center gap-2 text-sm font-medium text-gray-700"
    >
      Review
      <span
        data-review-track
        className={clsx(
          'relative inline-flex h-5 w-9 flex-shrink-0 rounded-full transition-colors',
          on ? 'bg-blue-600' : 'bg-gray-300'
        )}
      >
        <span
          className={clsx(
            'absolute top-0.5 h-4 w-4 rounded-full bg-white shadow transition-all',
            on ? 'left-[1.125rem]' : 'left-0.5'
          )}
        />
      </span>
    </button>
  )
}
