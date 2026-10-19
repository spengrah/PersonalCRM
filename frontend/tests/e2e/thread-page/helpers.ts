import { expect, type APIRequestContext, type Page } from '@playwright/test'
import type { ThreadReviewResponse } from '../../../src/types/generated/contact'
import type { FlagKind } from '../../../src/types/generated/judgment'
import {
  API_BASE_URL,
  API_HEADERS,
  type SeedBehaviorResult,
  type TestAPI,
} from '../helpers/test-api'
import {
  COMPARED_METHODS,
  entity,
  fetchThreads,
  readThread,
  type SeedFlag,
  type SeedClearing,
  type SeedUnjudged,
} from '../helpers/bench'

export interface SeedReply {
  question_id: string
  contact_id: string
  outcome: 'i_owe_reply' | 'they_owe_reply' | 'nobody_owes'
  confidence: number
}

/** A judgment the thread page tests record: the shared seed's, plus ball-in-court answers. */
export interface NightSeed {
  cut_off: string
  window_start: string
  new_messages: string[]
  flags?: SeedFlag[]
  clearings?: SeedClearing[]
  ball_in_court?: SeedReply[]
  questions?: { id: string; probabilities: Record<string, number>; original_answer: string }[]
}

export interface ThreadRunSeed {
  interactionId: string
  nights: NightSeed[]
  unjudged?: Omit<SeedUnjudged, 'thread_interaction_id'>[]
}

/**
 * Records one run through the judgment store's testing-only route, as a method
 * would: for each thread one judgment per night, and the new messages no
 * judgment answered. Defaults to a historical run of the first compared method.
 */
export async function seedThreadsRun(
  request: APIRequestContext,
  threads: ThreadRunSeed[],
  run: { method_key?: string; from?: string; to?: string } = {}
): Promise<string> {
  const response = await request.post(`${API_BASE_URL}/api/v1/test/judgment-runs`, {
    headers: API_HEADERS,
    data: {
      kind: 'historical',
      method_key: run.method_key ?? COMPARED_METHODS.clef,
      method_version: 'v1',
      wording_version: 'w1',
      from: run.from ?? '2026-01-01T00:00:00Z',
      to: run.to ?? '2026-01-31T00:00:00Z',
      judgments: threads.flatMap(thread =>
        thread.nights.map(night => ({
          thread_interaction_id: thread.interactionId,
          context_kind: 'conversation',
          ...night,
        }))
      ),
      incomplete: [],
      unjudged: threads.flatMap(thread =>
        (thread.unjudged ?? []).map(u => ({ thread_interaction_id: thread.interactionId, ...u }))
      ),
    },
  })
  expect(response.status(), await response.text()).toBe(201)
  return (await response.json()).data.run_id
}

/** Records a run of one thread. */
export function seedRun(
  request: APIRequestContext,
  interactionId: string,
  run: {
    method_key?: string
    from?: string
    to?: string
    nights: NightSeed[]
    unjudged?: Omit<SeedUnjudged, 'thread_interaction_id'>[]
  }
): Promise<string> {
  const { nights, unjudged, ...window } = run
  return seedThreadsRun(request, [{ interactionId, nights, unjudged }], window)
}

/** A flag decision of a message. */
export const flag = (message_key: string, kind: FlagKind, probability: number): SeedFlag => ({
  message_key,
  kind,
  probability,
})

/** An instant just after a message was sent: the cut-off of the night that message ends. */
export function afterwards(sentAt: string): string {
  return new Date(new Date(sentAt).getTime() + 30_000).toISOString()
}

/** The review world's chats of the three contacts, with their messages in thread order. */
export async function reviewWorld(testApi: TestAPI, request: APIRequestContext) {
  const seeded: SeedBehaviorResult = await testApi.seedBehavior('IXN-080')
  const [ana, ben, cai] = ['ana', 'ben', 'cai'].map(handle => entity(seeded, handle))
  const chat = async (handle: string, member: { id: string }) => {
    const interaction = entity(seeded, handle)
    const { data } = await fetchThreads(request, { contact_id: member.id, limit: 100 })
    const thread = data.items.find(row => row.title?.endsWith(handle))!
    expect(thread, `the ${handle} chat is in the contact's threads`).toBeTruthy()
    const { messages } = await readThread(request, thread.thread_id)
    return { interactionId: interaction.id, thread, messages }
  }
  return {
    seeded,
    ana,
    ben,
    cai,
    board: await chat('board', ana),
    side: await chat('side', ana),
    quiet: await chat('quiet', cai),
  }
}

/** The thread's review in a run, as the API reads it. */
export async function reviewOf(
  request: APIRequestContext,
  threadId: string,
  runId: string
): Promise<ThreadReviewResponse> {
  const read = await readThread(request, threadId, { run_id: runId })
  expect(read.review, 'the run reviews the thread').toBeTruthy()
  return read.review!
}

/** Opens a page with Review set, so a test starts on or off without clicking the switch. */
export async function startWithReview(page: Page, on: boolean) {
  await page.addInitScript(
    ([value]) => {
      // Once per tab: the test's own clicks on the switch then stand across reloads.
      if (!window.sessionStorage.getItem('review-seeded')) {
        window.localStorage.setItem('crm:review', value)
        window.sessionStorage.setItem('review-seeded', '1')
      }
    },
    [on ? 'on' : 'off']
  )
}

export function threadUrl(threadId: string, query: Record<string, string> = {}): string {
  const search = new URLSearchParams(query).toString()
  return `/interactions/threads/${threadId}${search ? `?${search}` : ''}`
}

export interface NightRange {
  /** Indexes of the thread's first and last new message of the night, inclusive. */
  from: number
  to: number
  /** Index of the first message the window reads; the night's first new message by default. */
  window?: number
  flags?: SeedFlag[]
  clearings?: SeedClearing[]
  ball_in_court?: SeedReply[]
  questions?: NightSeed['questions']
}

/** Judgments of a thread's nights: each night's new messages are a range of its messages. */
export function nightsOf(
  messages: { message_key: string; sent_at: string }[],
  ranges: NightRange[]
): NightSeed[] {
  return ranges.map(({ from, to, window, ...rest }) => ({
    cut_off: afterwards(messages[to].sent_at),
    window_start: messages[window ?? from].message_key,
    new_messages: messages.slice(from, to + 1).map(m => m.message_key),
    ...rest,
  }))
}

/** The keys of the thread's messages as the page draws them, in document order. */
export async function shownKeys(page: Page): Promise<(string | null)[]> {
  return page
    .locator('[data-review-timeline] [data-message-key]')
    .evaluateAll(nodes => nodes.map(node => node.getAttribute('data-message-key')))
}

/** A review save as the browser sent it. */
export interface SavedRequest {
  kind: 'flag' | 'clearing' | 'reply' | 'nones' | 'close'
  body: unknown
}

const SAVE_PATHS: [SavedRequest['kind'], RegExp][] = [
  ['flag', /\/threads\/[^/]+\/flag-labels/],
  ['clearing', /\/threads\/[^/]+\/clearing-labels/],
  ['reply', /\/judgment-outcomes\/[^/]+\/review/],
  ['nones', /\/judgments\/[^/]+\/confirm-nones/],
  ['close', /\/threads\/[^/]+\/close/],
]

/**
 * Holds every review save's response from the server until released, and
 * records the saves in the order the browser sent and the server answered them.
 * `events` lists "sent:{kind}" and "answered:{kind}" in the order they happened.
 */
export async function holdSaves(page: Page) {
  const events: string[] = []
  const sent: SavedRequest[] = []
  let release!: () => void
  const released = new Promise<void>(resolve => (release = resolve))
  await page.route(
    url => SAVE_PATHS.some(([, path]) => path.test(url.pathname)),
    async route => {
      const request = route.request()
      const kind = SAVE_PATHS.find(([, path]) => path.test(new URL(request.url()).pathname))![0]
      events.push(`sent:${kind}`)
      sent.push({ kind, body: request.postDataJSON() })
      await released
      const response = await route.fetch()
      events.push(`answered:${kind}`)
      await route.fulfill({ response })
    }
  )
  return { events, sent, release }
}
