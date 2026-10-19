'use client'

import { useId } from 'react'
import { clsx } from 'clsx'
import type { InteractionType } from '@/lib/bench-api'
import type { InteractionTypeOptionResponse } from '@/types/generated/contact'

interface TypeFilterProps {
  /** The types the server offers, from its one source-to-type mapping. */
  options: InteractionTypeOptionResponse[]
  value: InteractionType | undefined
  onChange: (type: InteractionType | undefined) => void
}

/** The segmented Type control the Interactions and Threads tabs share: All, then each offered type. */
export function TypeFilter({ options, value, onChange }: TypeFilterProps) {
  const labelId = useId()
  return (
    <div className="flex flex-col gap-1 text-sm text-gray-700">
      <span id={labelId}>Type</span>
      <div
        role="group"
        aria-labelledby={labelId}
        className="inline-flex flex-wrap overflow-hidden rounded-md border border-gray-300"
      >
        {[{ key: undefined, label: 'All' }, ...options].map(option => {
          const active = value === option.key
          return (
            <button
              key={option.key ?? 'all'}
              type="button"
              aria-pressed={active}
              onClick={() => onChange(option.key)}
              className={clsx(
                'border-l border-gray-300 px-3 py-1 text-sm first:border-l-0',
                active ? 'bg-gray-900 text-white' : 'bg-white text-gray-600 hover:bg-gray-50'
              )}
            >
              {option.label}
            </button>
          )
        })}
      </div>
    </div>
  )
}
