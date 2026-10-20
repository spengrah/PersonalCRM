import type { InteractionType, ThreadListFilters } from '@/lib/bench-api'

/** The Threads list's address: /interactions?tab=threads&type=&contact=&run=&page=. */
export function listFiltersFrom(params: URLSearchParams): ThreadListFilters {
  const page = Number(params.get('page'))
  return {
    type: (params.get('type') as InteractionType | null) || undefined,
    contact_id: params.get('contact') || undefined,
    run_id: params.get('run') || undefined,
    page: Number.isInteger(page) && page > 1 ? page : undefined,
  }
}

function query(filters: ThreadListFilters, tab: boolean): string {
  const params = new URLSearchParams()
  if (tab) params.set('tab', 'threads')
  if (filters.type) params.set('type', filters.type)
  if (filters.contact_id) params.set('contact', filters.contact_id)
  if (filters.run_id) params.set('run', filters.run_id)
  if (filters.page && filters.page > 1) params.set('page', String(filters.page))
  return params.toString()
}

export function threadsAddress(filters: ThreadListFilters): string {
  return `/interactions?${query(filters, true)}`
}

/** A thread page's address, carrying the list's filters so its pager walks the list as filtered. */
export function threadAddress(threadId: string, filters: ThreadListFilters): string {
  const search = query(filters, false)
  return search
    ? `/interactions/threads/${threadId}?${search}`
    : `/interactions/threads/${threadId}`
}
