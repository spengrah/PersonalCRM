// Single source of truth for contact-list URL parameters.
//
// The sort/filter/search state of the contacts list travels through URLs in
// three places: the list page itself, list→detail links, and the detail
// page's prev/next + back-to-list navigation. Every reader and writer of
// those params must go through this module so the three stay symmetric —
// a param dropped by one builder silently breaks round-tripping.

export type SortField =
  | 'name'
  | 'location'
  | 'birthday'
  | 'last_contacted'
  | 'last_response_at'
  | 'contact_by'
  | 'cadence'
export type SortOrder = 'asc' | 'desc'
export type CadenceFilter = 'has_cadence' | 'no_cadence'
export type FollowupFilter = 'has_followup' | 'no_followup'

export const SORT_FIELDS: readonly SortField[] = [
  'name',
  'location',
  'birthday',
  'last_contacted',
  'last_response_at',
  'contact_by',
  'cadence',
]

export const DEFAULT_SORT_FIELD: SortField = 'cadence'
export const DEFAULT_SORT_ORDER: SortOrder = 'desc'

// The list's page size; also the divisor pageFromIndex uses to compute which
// list page holds the contact in view. One constant so the two can never drift.
export const CONTACTS_PAGE_SIZE = 20

// Order applied when a column is first selected: most-frequent/most-recent
// first for cadence and last_response_at, ascending for everything else.
export function defaultOrderFor(field: SortField): SortOrder {
  return field === 'cadence' || field === 'last_response_at' ? 'desc' : 'asc'
}

// The list context that travels through URLs. sort/order are always
// resolved (defaults applied); the filters and search are optional.
export interface ContactListContext {
  sort: SortField
  order: SortOrder
  search?: string
  cadence_filter?: CadenceFilter
  followup_filter?: FollowupFilter
}

export const DEFAULT_LIST_CONTEXT: ContactListContext = {
  sort: DEFAULT_SORT_FIELD,
  order: DEFAULT_SORT_ORDER,
}

function isSortField(value: string): value is SortField {
  return (SORT_FIELDS as readonly string[]).includes(value)
}

// parseListContext reads the list context from URL search params, falling
// back to defaults for missing or invalid values. Accepts anything with a
// URLSearchParams-style get() so both next/navigation's
// ReadonlyURLSearchParams and plain URLSearchParams work.
export function parseListContext(searchParams: {
  get(name: string): string | null
}): ContactListContext {
  const rawSort = searchParams.get('sort') ?? ''
  const rawOrder = searchParams.get('order') ?? ''
  const search = searchParams.get('search') ?? undefined
  const rawCadence = searchParams.get('cadence_filter') ?? ''
  const rawFollowup = searchParams.get('followup_filter') ?? ''

  return {
    sort: isSortField(rawSort) ? rawSort : DEFAULT_SORT_FIELD,
    order: rawOrder === 'asc' || rawOrder === 'desc' ? rawOrder : DEFAULT_SORT_ORDER,
    ...(search ? { search } : {}),
    ...(rawCadence === 'has_cadence' || rawCadence === 'no_cadence'
      ? { cadence_filter: rawCadence }
      : {}),
    ...(rawFollowup === 'has_followup' || rawFollowup === 'no_followup'
      ? { followup_filter: rawFollowup }
      : {}),
  }
}

// listContextSearchParams serializes a context back to URL params. sort and
// order are always written (navigation order must match list order even at
// defaults); search and filters only when present.
export function listContextSearchParams(context: ContactListContext): URLSearchParams {
  const params = new URLSearchParams()
  params.set('sort', context.sort)
  params.set('order', context.order)
  if (context.search) params.set('search', context.search)
  if (context.cadence_filter) params.set('cadence_filter', context.cadence_filter)
  if (context.followup_filter) params.set('followup_filter', context.followup_filter)
  return params
}

// buildContactListUrl builds the list page URL carrying the full context
// (the detail page's "back to list" target). The optional page is appended as
// &page=N only when it is a finite integer > 1 — page 1 is the canonical bare
// URL, so existing links and the sort/filter reset path stay byte-identical.
export function buildContactListUrl(context: ContactListContext, page?: number): string {
  const params = listContextSearchParams(context)
  if (page !== undefined && Number.isInteger(page) && page > 1) {
    params.set('page', String(page))
  }
  return `/contacts?${params.toString()}`
}

// parseListPage reads the 1-based list page from URL search params, falling
// back to page 1 for a missing, malformed, zero, negative, or non-integer
// value. Page rides the URL directly and is NOT part of ContactListContext.
export function parseListPage(searchParams: { get(name: string): string | null }): number {
  const raw = Number(searchParams.get('page'))
  return Number.isInteger(raw) && raw > 0 ? raw : 1
}

// pageFromIndex maps a global 0-based list index to the 1-based list page that
// holds it. undefined when the id list has not resolved (index < 0) → the
// caller omits page → the list opens on page 1.
export function pageFromIndex(currentIndex: number): number | undefined {
  if (currentIndex < 0) return undefined
  return Math.floor(currentIndex / CONTACTS_PAGE_SIZE) + 1
}

// buildContactDetailUrl builds a detail page URL carrying the full context
// (list→detail links and prev/next navigation), plus an optional one-shot
// action param consumed by the detail page.
export function buildContactDetailUrl(
  context: ContactListContext,
  contactId: string,
  action?: 'edit' | 'merge'
): string {
  const params = listContextSearchParams(context)
  if (action) params.set('action', action)
  return `/contacts/${contactId}?${params.toString()}`
}
