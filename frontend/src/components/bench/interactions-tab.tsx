'use client'

import { useState } from 'react'
import { Pagination } from '@/components/ui/pagination'
import { MergeInteractionsDialog } from '@/components/corrections/merge-interactions-dialog'
import { RemoveInteractionDialog } from '@/components/corrections/remove-interaction-dialog'
import type { TimelineIdentity, TimelineRowAction } from '@/components/timeline'
import { useAllInteractions } from '@/hooks/use-bench'
import type { AllInteractionsFilters, InteractionType } from '@/lib/bench-api'
import type { AllInteractionItemResponse } from '@/types/generated/contact'
import { InteractionFilters } from './interaction-filters'
import { InteractionRow } from './interaction-row'

/** The Interactions tab's address: /interactions?contact=&type=&page=. */
export function interactionFiltersFrom(params: URLSearchParams): AllInteractionsFilters {
  const page = Number(params.get('page'))
  return {
    contact_id: params.get('contact') || undefined,
    type: (params.get('type') as InteractionType | null) || undefined,
    page: Number.isInteger(page) && page > 1 ? page : undefined,
  }
}

export function interactionsAddress(filters: AllInteractionsFilters): string {
  const params = new URLSearchParams()
  if (filters.contact_id) params.set('contact', filters.contact_id)
  if (filters.type) params.set('type', filters.type)
  if (filters.page && filters.page > 1) params.set('page', String(filters.page))
  const query = params.toString()
  return query ? `/interactions?${query}` : '/interactions'
}

interface InteractionsTabProps {
  filters: AllInteractionsFilters
  onChange: (next: AllInteractionsFilters) => void
}

/**
 * The Interactions tab: every interaction across contacts, for correcting
 * frequency data (merge and remove, through each row's menu). It shows no
 * judgment, whether Review is on or off.
 */
export function InteractionsTab({ filters, onChange }: InteractionsTabProps) {
  const list = useAllInteractions(filters)
  const rows = list.data?.data?.items ?? []
  const pagination = list.data?.meta?.pagination

  // Corrections, as the contact history offers them, each in its row's
  // participation context.
  const [removing, setRemoving] = useState<AllInteractionItemResponse | null>(null)
  const [mergeSelection, setMergeSelection] = useState<string[] | null>(null)
  const [reviewingMerge, setReviewingMerge] = useState(false)
  const rowOf = (identity: TimelineIdentity) =>
    rows.find(row => row.item.identity.id === identity.id)
  const toggleMergeSelection = (identity: TimelineIdentity) =>
    setMergeSelection(current =>
      current?.includes(identity.id)
        ? current.filter(id => id !== identity.id)
        : [...(current ?? []), identity.id]
    )
  const actions: TimelineRowAction[] = [
    { key: 'remove', label: 'Remove', onSelect: identity => setRemoving(rowOf(identity) ?? null) },
    {
      key: 'merge',
      label: 'Merge',
      onSelect: identity =>
        setMergeSelection(current =>
          current?.includes(identity.id) ? current : [...(current ?? []), identity.id]
        ),
    },
  ]
  const mergeRows = (mergeSelection ?? [])
    .map(id => rows.find(row => row.item.identity.id === id))
    .filter((row): row is AllInteractionItemResponse => Boolean(row))
  const endMerge = () => {
    setReviewingMerge(false)
    setMergeSelection(null)
  }
  const filtered = Boolean(filters.contact_id || filters.type)

  return (
    <>
      <InteractionFilters
        filters={filters}
        typeOptions={list.data?.data?.type_options ?? []}
        onChange={onChange}
      />
      {mergeSelection && (
        <div
          role="region"
          aria-label="Merge selection"
          className="flex flex-wrap items-center gap-3 border-b border-gray-200 bg-blue-50 px-4 py-3 sm:px-6"
        >
          <p className="flex-1 text-sm text-blue-900">
            Select the interactions to merge. {mergeRows.length} selected.
          </p>
          <button
            type="button"
            onClick={endMerge}
            className="rounded border border-gray-300 bg-white px-3 py-1.5 text-sm font-medium text-gray-700"
          >
            Cancel
          </button>
          <button
            type="button"
            disabled={mergeRows.length < 2}
            onClick={() => setReviewingMerge(true)}
            className="rounded bg-blue-600 px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50"
          >
            Review merge
          </button>
        </div>
      )}
      {list.isError ? (
        <p className="p-6 text-sm text-red-700">Failed to load interactions.</p>
      ) : list.isPending ? (
        <p className="p-6 text-sm text-gray-500">Loading interactions…</p>
      ) : rows.length === 0 ? (
        <p className="p-6 text-sm text-gray-500">
          No interactions match these filters
          {filtered && (
            <>
              {' · '}
              <button
                type="button"
                onClick={() => onChange({})}
                className="text-blue-600 hover:text-blue-800 hover:underline"
              >
                Clear filters
              </button>
            </>
          )}
        </p>
      ) : (
        <div role="list" aria-label="Interactions" className="divide-y divide-gray-200">
          {rows.map(row => (
            <InteractionRow
              key={`${row.item.identity.kind}:${row.item.identity.id}`}
              row={row}
              actions={actions}
              selection={
                mergeSelection
                  ? {
                      selected: mergeSelection.includes(row.item.identity.id),
                      onToggle: toggleMergeSelection,
                    }
                  : undefined
              }
            />
          ))}
        </div>
      )}
      {pagination && pagination.pages > 1 && (
        <div className="border-t border-gray-200 px-4 py-4 sm:px-6">
          <Pagination
            page={pagination.page}
            pages={pagination.pages}
            total={pagination.total}
            noun="interactions"
            onPageChange={page => onChange({ ...filters, page })}
          />
        </div>
      )}
      {removing && (
        <RemoveInteractionDialog
          item={removing.item}
          context={{ contactId: removing.context_contact_id }}
          onClose={() => setRemoving(null)}
        />
      )}
      {reviewingMerge && mergeRows.length >= 2 && (
        <MergeInteractionsDialog
          items={mergeRows.map(row => row.item)}
          context={{ contactId: mergeRows[0].context_contact_id }}
          onClose={() => setReviewingMerge(false)}
          onMerged={endMerge}
        />
      )}
    </>
  )
}
