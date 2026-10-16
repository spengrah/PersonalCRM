import { expect, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import type {
  AllInteractionItemResponse,
  AllInteractionsResponse,
  TimelineContentResponse,
} from '../../../src/types/generated/contact'
import { API_BASE_URL, API_HEADERS, type SeedBehaviorResult } from './test-api'
import { SLOW_TIMEOUT_MS } from './timeouts'

/**
 * The method key the E2E backend designates as the production method
 * (DECISION_PRODUCTION_METHOD), so its judgments of current content change
 * live state.
 */
export const PRODUCTION_METHOD = 'bench-production'

export interface SeedOutcome {
  question_id: string
  contact_id?: string
  direction?: 'mine' | 'theirs'
  outcome: string
  confidence: number
  due_date?: string
}

export interface SeedJudgment {
  /** An interaction's observation id or a row's interaction node id: its thread is judged. */
  thread_interaction_id: string
  cut_off: string
  input_hash?: string
  context_kind?: 'conversation' | 'meeting'
  questions?: { id: string; probabilities: Record<string, number>; original_answer: string }[]
  ball_in_court?: SeedOutcome[]
  commitments?: SeedOutcome[]
  big_moment?: SeedOutcome
}

export interface SeedRun {
  kind?: 'scheduled' | 'historical' | 'rerun'
  method_key?: string
  method_version?: string
  from?: string
  to?: string
  judgments?: SeedJudgment[]
  incomplete?: { thread_interaction_id: string; cut_off: string; reason: string }[]
  unfinished?: boolean
}

/**
 * Records one judgment run through the judgment store's testing-only route,
 * as a method would. Defaults: a scheduled run of the production method.
 */
export async function seedJudgmentRun(
  request: APIRequestContext,
  run: SeedRun
): Promise<{ run_id: string; threads: string[] }> {
  const response = await request.post(`${API_BASE_URL}/api/v1/test/judgment-runs`, {
    headers: API_HEADERS,
    data: {
      kind: 'scheduled',
      method_key: PRODUCTION_METHOD,
      method_version: 'v1',
      wording_version: 'w1',
      judgments: [],
      incomplete: [],
      ...run,
      ...(run.judgments && {
        judgments: run.judgments.map(j => ({ context_kind: 'conversation', ...j })),
      }),
    },
  })
  expect(response.status(), await response.text()).toBe(201)
  return (await response.json()).data
}

/** One page of the all-interactions view as the API gives it. */
export async function fetchAllInteractions(
  request: APIRequestContext,
  query: Record<string, string | number>
): Promise<{ data: AllInteractionsResponse; total: number; pages: number }> {
  const params = new URLSearchParams(Object.entries(query).map(([k, v]) => [k, String(v)]))
  const response = await request.get(`${API_BASE_URL}/api/v1/interactions?${params}`, {
    headers: API_HEADERS,
  })
  expect(response.ok()).toBe(true)
  const body = await response.json()
  return { data: body.data, total: body.meta.pagination.total, pages: body.meta.pagination.pages }
}

/** Every row a contact takes part in, as the API orders them. */
export async function contactRows(
  request: APIRequestContext,
  contactId: string
): Promise<AllInteractionItemResponse[]> {
  return (await fetchAllInteractions(request, { contact_id: contactId, limit: 100 })).data.items
}

/** The content of a row, read in its context contact's participation. */
export async function rowContent(
  request: APIRequestContext,
  row: AllInteractionItemResponse
): Promise<TimelineContentResponse> {
  const { kind, id } = row.item.identity
  const response = await request.get(
    `${API_BASE_URL}/api/v1/timeline-items/${kind}/${id}/content?contact_id=${row.context_contact_id}`,
    { headers: API_HEADERS }
  )
  expect(response.ok()).toBe(true)
  return (await response.json()).data
}

/** A row's messages, oldest first. */
export async function rowMessages(request: APIRequestContext, row: AllInteractionItemResponse) {
  return (await rowContent(request, row)).members
    .flatMap(member => member.messages)
    .sort((a, b) => new Date(a.sent_at).getTime() - new Date(b.sent_at).getTime())
}

export function entity(seeded: SeedBehaviorResult, handle: string): { id: string; name: string } {
  const e = seeded.entities[handle]
  expect(e, `declared handle ${handle} should be in the manifest`).toBeTruthy()
  return e
}

/** The view's row for an interaction identity. */
export function benchRow(page: Page, identityId: string): Locator {
  return page
    .getByRole('list', { name: 'Interactions' })
    .locator(`[data-interaction-id="${identityId}"]`)
}

/** Expands a row and returns its drill-down. */
export async function expandRow(row: Locator): Promise<Locator> {
  await row.getByRole('button', { name: 'Expand content', exact: true }).click()
  const region = row.locator('[data-content-region]')
  await expect(region).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
  return region
}

/** A proposal's list by its heading. */
export function proposalList(proposal: Locator, title: string): Locator {
  return proposal.getByRole('group', { name: title, exact: true })
}

/** The one item of a list whose text starts with the given words. */
export function listItem(list: Locator, text: string): Locator {
  return list.locator('[data-item]').filter({
    has: list.page().locator('[data-item-text]', { hasText: text }),
  })
}

/** Today's calendar date as the server reads it (YYYY-MM-DD, UTC). */
export function isoDate(d: Date): string {
  return d.toISOString().slice(0, 10)
}

export interface SeededObligation {
  contact_id: string
  debtor: 'user' | 'contact'
  basis: 'reply' | 'commitment'
  kind: string
  due_date: string
  open: boolean
}

/** The obligations on a thread, open or closed, through the testing-only read. */
export async function fetchObligations(
  request: APIRequestContext,
  threadInteractionId: string
): Promise<SeededObligation[]> {
  const response = await request.get(
    `${API_BASE_URL}/api/v1/test/judgment-obligations?thread_interaction_id=${threadInteractionId}`,
    { headers: API_HEADERS }
  )
  expect(response.ok(), await response.text()).toBe(true)
  return (await response.json()).data
}

/** The local calendar date days from now (YYYY-MM-DD), as the server's "today" reads it. */
export function localDateFromNow(days: number): string {
  const d = new Date()
  d.setDate(d.getDate() + days)
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}
