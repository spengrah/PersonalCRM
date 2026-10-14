import { expect, type APIRequestContext } from '@playwright/test'
import type {
  InteractionListResponse,
  TimelineContentResponse,
  TimelineIdentityResponse,
} from '../../../src/types/generated/contact'
import { API_BASE_URL, API_HEADERS } from './test-api'

export type TimelineListItem = InteractionListResponse['items'][number]

/**
 * The contact's interaction list as the API gives it. `limit` and `page` pass
 * through to the endpoint; a spec that needs every item asks for a limit above
 * its seeded count.
 */
export async function fetchTimeline(
  request: APIRequestContext,
  contactId: string,
  query: { limit?: number; page?: number } = {}
): Promise<InteractionListResponse> {
  const params = new URLSearchParams()
  if (query.limit) params.set('limit', String(query.limit))
  if (query.page) params.set('page', String(query.page))
  const suffix = params.size > 0 ? `?${params}` : ''
  const response = await request.get(
    `${API_BASE_URL}/api/v1/contacts/${contactId}/interactions${suffix}`,
    { headers: API_HEADERS }
  )
  expect(response.ok()).toBe(true)
  return (await response.json()).data as InteractionListResponse
}

/** The content of one timeline item, read in the participation context of one contact. */
export async function fetchTimelineContent(
  request: APIRequestContext,
  identity: TimelineIdentityResponse,
  contactId: string
): Promise<TimelineContentResponse> {
  const response = await request.get(
    `${API_BASE_URL}/api/v1/timeline-items/${identity.kind}/${identity.id}/content?contact_id=${contactId}`,
    { headers: API_HEADERS }
  )
  expect(response.ok()).toBe(true)
  return (await response.json()).data as TimelineContentResponse
}

/** The items whose first member has the given source. */
export function itemsWithSource(items: TimelineListItem[], source: string): TimelineListItem[] {
  return items.filter(item => item.members[0]?.source === source)
}

/** The single item whose first member has the given source. */
export function itemWithSource(items: TimelineListItem[], source: string): TimelineListItem {
  const matches = itemsWithSource(items, source)
  expect(matches, `exactly one ${source} item`).toHaveLength(1)
  return matches[0]
}

/**
 * The items of one source that a declaration seeded `agoDays` before the
 * world's anchor. A seeded handle names an observation, not a timeline item,
 * so a spec reaches a row through what the declaration seeded: its source and
 * its age. Seeded ages differ by whole days, so a 12 hour window is exact.
 */
export function itemsAgoDays(
  items: TimelineListItem[],
  anchor: string,
  source: string,
  agoDays: number
): TimelineListItem[] {
  const target = new Date(anchor).getTime() - agoDays * 24 * 60 * 60 * 1000
  return itemsWithSource(items, source).filter(
    item => Math.abs(new Date(item.occurred_at).getTime() - target) < 12 * 60 * 60 * 1000
  )
}

/** The single such item. */
export function itemAgoDays(
  items: TimelineListItem[],
  anchor: string,
  source: string,
  agoDays: number
): TimelineListItem {
  const matches = itemsAgoDays(items, anchor, source, agoDays)
  expect(matches, `exactly one ${source} item ${agoDays} days before the anchor`).toHaveLength(1)
  return matches[0]
}
