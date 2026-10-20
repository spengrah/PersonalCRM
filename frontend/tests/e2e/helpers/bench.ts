import { expect, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import type {
  AllInteractionItemResponse,
  AllInteractionsResponse,
  ThreadListResponse,
  ThreadReadResponse,
  ThreadReviewResponse,
  ThreadRowResponse,
  TimelineContentResponse,
} from '../../../src/types/generated/contact'
import type { FlagKind, UnjudgedReason } from '../../../src/types/generated/judgment'
import { API_BASE_URL, API_HEADERS, type SeedBehaviorResult } from './test-api'
import { SLOW_TIMEOUT_MS } from './timeouts'

/**
 * The compared methods, the only ones whose runs the Run filter lists. The
 * backend's method registry fixes them; a run of any other key is never listed.
 */
export const COMPARED_METHODS = {
  clef: 'clef-masked',
  reasoned: 'clef-masked-reasoned',
  jev: 'jev-masked',
} as const

export interface SeedFlag {
  message_key: string
  kind: FlagKind
  probability: number
}

export interface SeedClearing {
  commitment_message_key: string
  /** The cut-off of the earlier judgment that flagged the commitment. */
  commitment_cut_off: string
  done: boolean
  probability: number
}

export interface SeedJudgment {
  /** An interaction's observation id or a row's interaction node id: its thread is judged. */
  thread_interaction_id: string
  cut_off: string
  input_hash?: string
  context_kind?: 'conversation' | 'meeting'
  /** The key of the first message its window reads. */
  window_start?: string
  /** The keys of the messages new at its cut-off, in thread order. */
  new_messages?: string[]
  flags?: SeedFlag[]
  clearings?: SeedClearing[]
}

export interface SeedUnjudged {
  thread_interaction_id: string
  cut_off: string
  message_key: string
  reason: UnjudgedReason
}

export interface SeedRun {
  kind?: 'scheduled' | 'historical' | 'rerun'
  method_key?: string
  method_version?: string
  from?: string
  to?: string
  judgments?: SeedJudgment[]
  unjudged?: SeedUnjudged[]
  unfinished?: boolean
}

/**
 * Records one judgment run through the judgment store's testing-only route,
 * as a method would. Defaults: a historical run of the first compared method.
 */
export async function seedJudgmentRun(
  request: APIRequestContext,
  run: SeedRun
): Promise<{ run_id: string; threads: string[] }> {
  const response = await request.post(`${API_BASE_URL}/api/v1/test/judgment-runs`, {
    headers: API_HEADERS,
    data: {
      kind: 'historical',
      method_key: COMPARED_METHODS.clef,
      method_version: 'v1',
      wording_version: 'w1',
      judgments: [],
      incomplete: [],
      unjudged: [],
      ...run,
      ...(run.judgments && {
        judgments: run.judgments.map(j => ({ context_kind: 'conversation', ...j })),
      }),
    },
  })
  expect(response.status(), await response.text()).toBe(201)
  return (await response.json()).data
}

/** One page of the Interactions tab's list as the API gives it. */
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

/** One page of the Threads list as the API gives it. */
export async function fetchThreads(
  request: APIRequestContext,
  query: Record<string, string | number>
): Promise<{ data: ThreadListResponse; total: number; pages: number }> {
  const params = new URLSearchParams(Object.entries(query).map(([k, v]) => [k, String(v)]))
  const response = await request.get(`${API_BASE_URL}/api/v1/threads?${params}`, {
    headers: API_HEADERS,
  })
  expect(response.ok(), await response.text()).toBe(true)
  const body = await response.json()
  return { data: body.data, total: body.meta.pagination.total, pages: body.meta.pagination.pages }
}

/** A thread as the API reads it, with the run's review of it given a run_id. */
export async function readThread(
  request: APIRequestContext,
  threadId: string,
  query: Record<string, string> = {}
): Promise<ThreadReadResponse> {
  const params = new URLSearchParams(query)
  const response = await request.get(`${API_BASE_URL}/api/v1/threads/${threadId}?${params}`, {
    headers: API_HEADERS,
  })
  expect(response.ok(), await response.text()).toBe(true)
  return (await response.json()).data
}

/**
 * The thread an interaction was recorded in, from the contact's threads: the
 * one list row naming the contact whose latest message is the interaction's.
 */
export async function threadOf(
  request: APIRequestContext,
  contactId: string,
  title: string | undefined
): Promise<ThreadRowResponse> {
  const { data } = await fetchThreads(request, { contact_id: contactId, limit: 100 })
  const thread = title ? data.items.find(row => row.title === title) : data.items[0]
  expect(thread, `a thread of the contact${title ? ` titled ${title}` : ''}`).toBeTruthy()
  return thread!
}

/** Labels a flag decision as the user would, so a thread reads as reviewed without driving the page. */
export async function labelFlag(
  request: APIRequestContext,
  threadId: string,
  runId: string,
  label: { message_key: string; kind: FlagKind; holds: boolean }
): Promise<ThreadReviewResponse> {
  const response = await request.post(
    `${API_BASE_URL}/api/v1/threads/${threadId}/flag-labels?run_id=${runId}`,
    { headers: API_HEADERS, data: label }
  )
  expect(response.ok(), await response.text()).toBe(true)
  return (await response.json()).data
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

/** The Interactions tab's row for an interaction identity. */
export function interactionRow(page: Page, identityId: string): Locator {
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

/** A judgment box's reply list by its heading. */
export function proposalList(box: Locator, title: string): Locator {
  return box.getByRole('group', { name: title, exact: true })
}

/** The one item of a list whose text starts with the given words. */
export function listItem(list: Locator, text: string): Locator {
  return list.locator('[data-item]').filter({
    has: list.page().locator('[data-item-text]', { hasText: text }),
  })
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

/**
 * A run's name as the Run filter and thread header read it: the given
 * "{method} · {month}" followed by the local time the run started.
 */
export function runNamed(methodAndMonth: string): RegExp {
  const escaped = methodAndMonth.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  return new RegExp(`^${escaped} · started [A-Z][a-z]{2} \\d{1,2}, \\d{1,2}:\\d{2}\\s[AP]M$`)
}
