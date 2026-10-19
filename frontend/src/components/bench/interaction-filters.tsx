'use client'

import { clsx } from 'clsx'
import { ContactFilter } from '@/components/threads/contact-filter'
import type { AllInteractionsFilters, InteractionType } from '@/lib/bench-api'
import type { InteractionTypeOptionResponse } from '@/types/generated/contact'

interface InteractionFiltersProps {
  filters: AllInteractionsFilters
  typeOptions: InteractionTypeOptionResponse[]
  onChange: (next: AllInteractionsFilters) => void
}

/**
 * The Interactions tab's two filters, and no other: the type as a segmented
 * control, then the contact. They combine, and every change returns to the
 * first page.
 */
export function InteractionFilters({ filters, typeOptions, onChange }: InteractionFiltersProps) {
  const set = (patch: Partial<AllInteractionsFilters>) =>
    onChange({ ...filters, ...patch, page: undefined })
  const filtered = Boolean(filters.type || filters.contact_id)

  return (
    <div className="flex flex-wrap items-end gap-4 border-b border-gray-200 px-4 py-4 sm:px-6">
      <div className="flex flex-col gap-1 text-sm text-gray-700">
        <span id="interaction-type-label">Type</span>
        <div
          role="group"
          aria-labelledby="interaction-type-label"
          className="inline-flex flex-wrap overflow-hidden rounded-md border border-gray-300"
        >
          {[{ key: undefined, label: 'All' }, ...typeOptions].map(option => {
            const active = filters.type === option.key
            return (
              <button
                key={option.key ?? 'all'}
                type="button"
                aria-pressed={active}
                onClick={() => set({ type: option.key as InteractionType | undefined })}
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
      <ContactFilter
        contactId={filters.contact_id}
        onChange={contactId => set({ contact_id: contactId })}
      />
      {filtered && (
        <button
          type="button"
          onClick={() => onChange({})}
          className="pb-1 text-sm text-blue-600 hover:text-blue-800 hover:underline"
        >
          Clear filters
        </button>
      )}
    </div>
  )
}
