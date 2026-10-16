'use client'

import { clsx } from 'clsx'
import { Select } from '@/components/ui/select'
import { useContact, useContacts } from '@/hooks/use-contacts'
import type { AllInteractionsFilters, InteractionType } from '@/lib/bench-api'
import type { InteractionTypeOptionResponse, JudgmentRunResponse } from '@/types/generated/contact'
import { runLabel } from './format'

interface BenchFiltersProps {
  filters: AllInteractionsFilters
  typeOptions: InteractionTypeOptionResponse[]
  /** Newest first. */
  runs: JudgmentRunResponse[]
  onChange: (next: AllInteractionsFilters) => void
}

/**
 * The view's three filters, and no other: the type as a segmented control,
 * then the contact and the run. They combine, and every change returns to the
 * first page.
 */
export function BenchFilters({ filters, typeOptions, runs, onChange }: BenchFiltersProps) {
  const contacts = useContacts({ limit: 1000, sort: 'name', order: 'asc' })
  // A contact filter set from a link may name a contact beyond the loaded
  // options; it stays selectable under its own name.
  const selected = useContact(filters.contact_id ?? '')
  const contactOptions = contacts.data?.contacts ?? []
  const missing =
    filters.contact_id &&
    selected.data &&
    !contactOptions.some(contact => contact.id === filters.contact_id)
      ? [selected.data]
      : []
  const set = (patch: Partial<AllInteractionsFilters>) =>
    onChange({ ...filters, ...patch, page: undefined })
  const filtered = Boolean(filters.type || filters.contact_id || filters.run_id)

  return (
    <div className="flex flex-wrap items-end gap-4 border-b border-gray-200 px-4 py-4 sm:px-6">
      <div className="flex flex-col gap-1 text-sm text-gray-700">
        <span id="bench-type-label">Type</span>
        <div
          role="group"
          aria-labelledby="bench-type-label"
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
      <div className="min-w-[12rem]">
        <Select
          id="bench-contact"
          label="Contact"
          value={filters.contact_id ?? ''}
          onChange={event => set({ contact_id: event.target.value || undefined })}
          className={clsx(filters.contact_id && 'border-blue-500')}
        >
          <option value="">All contacts</option>
          {[...missing, ...contactOptions].map(contact => (
            <option key={contact.id} value={contact.id}>
              {contact.full_name}
            </option>
          ))}
        </Select>
      </div>
      <div className="min-w-[16rem]">
        <Select
          id="bench-run"
          label="Run"
          value={filters.run_id ?? ''}
          onChange={event => set({ run_id: event.target.value || undefined })}
          className={clsx(filters.run_id && 'border-blue-500')}
        >
          <option value="">Any run, or none</option>
          {runs.map(run => (
            <option key={run.id} value={run.id}>
              {runLabel(run)}
            </option>
          ))}
        </Select>
      </div>
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
