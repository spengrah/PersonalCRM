'use client'

import { ContactFilter } from '@/components/threads/contact-filter'
import { TypeFilter } from '@/components/threads/type-filter'
import type { AllInteractionsFilters } from '@/lib/bench-api'
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
      <TypeFilter options={typeOptions} value={filters.type} onChange={type => set({ type })} />
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
