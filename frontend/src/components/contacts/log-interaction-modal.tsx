'use client'

import { useState, useEffect, useRef } from 'react'
import { useCreateInteraction } from '@/hooks/use-interactions'
import { Button } from '@/components/ui/button'
import { X } from 'lucide-react'
import type { InteractionDirection } from '@/lib/interactions-api'

interface LogInteractionModalProps {
  contactId: string
  contactName: string
  onClose: () => void
}

// today returns a YYYY-MM-DD string from the browser's wall clock for the
// date input's `max` attribute (cosmetic guard — the backend rejects future
// occurred_at regardless). When the user does NOT change the date input we
// omit occurred_at from the payload entirely so the backend can stamp it
// with accelerated.GetCurrentTime() — important in dev mode where the
// browser's wall clock and the backend's accelerated clock can drift.
function todayString(): string {
  const d = new Date()
  const yyyy = d.getFullYear()
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return `${yyyy}-${mm}-${dd}`
}

export function LogInteractionModal({ contactId, contactName, onClose }: LogInteractionModalProps) {
  const today = todayString()
  const [direction, setDirection] = useState<InteractionDirection>('mutual')
  // Default the visible value to today. userChangedDate tracks whether
  // the user explicitly picked a different value: if false at submit, we
  // omit occurred_at from the payload so the backend stamps the
  // interaction with accelerated.GetCurrentTime() instead of the
  // browser's wall-clock midnight.
  const [date, setDate] = useState<string>(today)
  const [userChangedDate, setUserChangedDate] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const initialFocusRef = useRef<HTMLButtonElement>(null)

  const createInteraction = useCreateInteraction()

  // Focus the default direction option on mount.
  useEffect(() => {
    initialFocusRef.current?.focus()
  }, [])

  // Handle escape key
  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [onClose])

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)

    // Build the request body. Omit occurred_at entirely if the user did
    // not change the date — this lets the backend stamp the interaction
    // with accelerated.GetCurrentTime(), which is the right thing in dev
    // mode where the wall clock and accelerated clock can drift. When the
    // user explicitly picks a date, send it (the YYYY-MM-DD value is
    // converted to a UTC midnight timestamp for the API).
    const body =
      userChangedDate && date
        ? { direction, occurred_at: new Date(date + 'T00:00:00Z').toISOString() }
        : { direction }

    try {
      await createInteraction.mutateAsync({ contactId, data: body })
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to log interaction')
    }
  }

  const directionOptions: { value: InteractionDirection; label: string; symbol: string }[] = [
    { value: 'mutual', label: 'Mutual', symbol: '⇆' },
    { value: 'outbound', label: 'Outbound', symbol: '↗' },
    { value: 'inbound', label: 'Inbound', symbol: '↙' },
  ]

  return (
    <div
      className="fixed inset-0 bg-black/30 backdrop-blur-sm z-50 flex items-start justify-center overflow-y-auto pt-4 sm:pt-20 px-4"
      onClick={e => {
        if (e.target === e.currentTarget) onClose()
      }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="log-interaction-title"
    >
      <div className="bg-white rounded-lg shadow-xl max-w-lg w-full mb-4">
        {/* Header */}
        <div className="px-4 py-3 border-b border-gray-200 flex items-center justify-between">
          <h2 id="log-interaction-title" className="text-lg font-medium text-gray-900">
            Log Interaction with {contactName}
          </h2>
          <button
            type="button"
            onClick={onClose}
            className="text-gray-400 hover:text-gray-600 p-1 rounded-full hover:bg-gray-100"
            aria-label="Close"
          >
            <X className="w-5 h-5" />
          </button>
        </div>

        <form onSubmit={handleSubmit}>
          <div className="px-4 py-4 space-y-4">
            {/* Direction picker */}
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Direction</label>
              <div
                className="inline-flex rounded-md shadow-sm"
                role="group"
                aria-label="Interaction direction"
              >
                {directionOptions.map((opt, idx) => {
                  const isSelected = direction === opt.value
                  const base =
                    'inline-flex items-center gap-1 px-3 py-2 text-sm font-medium border border-gray-300 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:z-10'
                  const position =
                    idx === 0
                      ? 'rounded-l-md'
                      : idx === directionOptions.length - 1
                        ? 'rounded-r-md -ml-px'
                        : '-ml-px'
                  const tone = isSelected
                    ? 'bg-blue-600 text-white border-blue-600'
                    : 'bg-white text-gray-700 hover:bg-gray-50'
                  return (
                    <button
                      key={opt.value}
                      ref={opt.value === 'mutual' ? initialFocusRef : undefined}
                      type="button"
                      aria-pressed={isSelected}
                      onClick={() => setDirection(opt.value)}
                      className={`${base} ${position} ${tone}`}
                    >
                      <span aria-hidden="true">{opt.symbol}</span>
                      <span>{opt.label}</span>
                    </button>
                  )
                })}
              </div>
            </div>

            {/* Date picker */}
            <div>
              <label
                htmlFor="log-interaction-date"
                className="block text-sm font-medium text-gray-700 mb-1"
              >
                Date
              </label>
              <input
                id="log-interaction-date"
                type="date"
                value={date}
                max={today}
                onChange={e => {
                  setDate(e.target.value)
                  setUserChangedDate(true)
                }}
                className="block rounded-md border border-gray-300 px-3 py-2 shadow-sm focus:border-blue-500 focus:ring-blue-500 text-sm text-gray-900"
                data-testid="log-interaction-date-input"
              />
              <p className="mt-1 text-xs text-gray-500">
                Defaults to today. Cannot be in the future.
              </p>
            </div>

            {/* Error */}
            {error && (
              <div className="text-sm text-red-600 bg-red-50 px-3 py-2 rounded-md">{error}</div>
            )}
          </div>

          {/* Footer */}
          <div className="px-4 py-3 border-t border-gray-200 flex justify-end gap-3">
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" loading={createInteraction.isPending}>
              Log
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}
