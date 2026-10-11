import { test, expect, type APIRequestContext } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import {
  API_BASE_URL,
  API_HEADERS,
  createTestAPI,
  TestAPI,
  type SeedBehaviorResult,
} from './helpers/test-api'

interface ContentMessage {
  id: string
  sender: string
  sent_at: string
  body: string
  venue_key: string
}

interface MeetingNote {
  title?: string
  summary?: string
  memo?: string
}

interface InteractionContent {
  kind: string
  messages: ContentMessage[]
  meeting_notes: MeetingNote[]
}

const handleId = (seeded: SeedBehaviorResult, handle: string): string => {
  const entity = seeded.entities[handle]
  expect(entity, `declared handle ${handle} should be in the manifest`).toBeTruthy()
  return entity.id
}

const handleName = (seeded: SeedBehaviorResult, handle: string): string => {
  const entity = seeded.entities[handle]
  expect(entity, `declared handle ${handle} should be in the manifest`).toBeTruthy()
  return entity.name
}

async function fetchContent(
  request: APIRequestContext,
  interactionId: string
): Promise<InteractionContent> {
  const response = await request.get(
    `${API_BASE_URL}/api/v1/interactions/${interactionId}/content`,
    { headers: API_HEADERS }
  )
  expect(response.ok()).toBe(true)
  return (await response.json()).data as InteractionContent
}

async function fetchContactName(request: APIRequestContext, contactId: string): Promise<string> {
  const response = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
    headers: API_HEADERS,
  })
  expect(response.ok()).toBe(true)
  const name = (await response.json()).data.full_name as string
  expect(name, 'seeded contact should have a name to resolve senders against').toBeTruthy()
  return name
}

function expectedDateTime(iso: string): string {
  return new Intl.DateTimeFormat('en-US', {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  }).format(new Date(iso))
}

test.describe('Interactions Drill-down @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IXN-004.full-plaintext-bodies, IXN-004.sender-and-timestamp, IXN-004.all-speakers-in-group, IXN-004.expand-in-place-no-nav, IXN-004.evidence-scope-disclosed
  test('expands a group thread to the full multi-speaker plaintext evidence', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-004')
    const contactId = handleId(seeded, 'subject')
    const speakerId = handleId(seeded, 'speaker')
    const interactionId = handleId(seeded, 'group-thread')
    await page.goto(`/contacts/${contactId}`)

    const interactions = page.getByRole('region', { name: 'Interactions' })
    const row = interactions.locator(`[data-interaction-id="${interactionId}"]`)
    await expect(row.getByText('2 messages', { exact: true })).toBeVisible()
    const urlBeforeExpand = page.url()
    await row.getByRole('button', { name: 'Expand content', exact: true }).click()

    const content = await fetchContent(request, interactionId)
    const region = row.locator('[data-content-region]')
    await expect(region).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    const messageNodes = region.locator('[data-message-id]')
    await expect(messageNodes).toHaveCount(content.messages.length, { timeout: SLOW_TIMEOUT_MS })
    await expect(
      messageNodes.evaluateAll(nodes => nodes.map(node => node.getAttribute('data-message-id')))
    ).resolves.toEqual(content.messages.map(message => message.id))

    for (let index = 0; index < content.messages.length; index += 1) {
      const message = content.messages[index]
      const node = messageNodes.nth(index)
      await expect(node.locator('[data-message-body]')).toHaveText(message.body, {
        timeout: SLOW_TIMEOUT_MS,
      })
      await expect(node.locator('[data-message-sender]')).toHaveText(message.sender, {
        timeout: SLOW_TIMEOUT_MS,
      })
      await expect(node.locator('[data-message-timestamp]')).toHaveText(
        expectedDateTime(message.sent_at),
        { timeout: SLOW_TIMEOUT_MS }
      )
      if (message.body.includes('<b>not-markup</b>')) {
        await expect(region.locator('b')).toHaveCount(0, { timeout: SLOW_TIMEOUT_MS })
      }
    }

    // Senders resolve to the CRM's own contact names, not to the raw peer
    // identifiers gchat carries. Pinned against the contacts fetched by id, so
    // a regression to handles fails here rather than silently rendering them.
    const [subjectName, speakerName] = await Promise.all([
      fetchContactName(request, contactId),
      fetchContactName(request, speakerId),
    ])
    const renderedSenders = await region.locator('[data-message-sender]').allTextContents()
    expect(new Set(renderedSenders)).toEqual(new Set([subjectName, speakerName]))

    // The row's count and the evidence on screen are drawn from different
    // sets — recorded-against-the-interaction vs the surrounding thread
    // window — so when they differ the drill-down states both.
    const scope = region.locator('[data-evidence-scope]')
    await expect(scope).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
    await expect(scope).toContainText(`Showing ${content.messages.length} messages`, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(scope).toContainText('2 recorded for this interaction', {
      timeout: SLOW_TIMEOUT_MS,
    })

    const senders = new Set(renderedSenders)
    expect(senders.size).toBe(2)
    const senderCounts = new Map<string, number>()
    for (const message of content.messages) {
      senderCounts.set(message.sender, (senderCounts.get(message.sender) ?? 0) + 1)
    }
    expect([...senderCounts.values()].sort()).toEqual([1, 2])
    expect(senderCounts.size).toBe(2)
    for (const [sender, count] of senderCounts) {
      await expect(region.locator('[data-message-sender]', { hasText: sender })).toHaveCount(
        count,
        {
          timeout: SLOW_TIMEOUT_MS,
        }
      )
    }
    await expect(page).toHaveURL(urlBeforeExpand, { timeout: SLOW_TIMEOUT_MS })
    await expect(page.getByRole('dialog')).toHaveCount(0, { timeout: SLOW_TIMEOUT_MS })
    await expect(row).toBeVisible({ timeout: SLOW_TIMEOUT_MS })

    await row.getByRole('button', { name: 'Collapse content', exact: true }).click()
    await expect(row.locator('[data-content-region]')).toHaveCount(0, { timeout: SLOW_TIMEOUT_MS })
    await expect(row.getByRole('button', { name: 'Expand content', exact: true })).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page).toHaveURL(urlBeforeExpand, { timeout: SLOW_TIMEOUT_MS })
  })

  // spec: IXN-005.summary-and-memo-shown, IXN-005.all-linked-notes-shown, IXN-005.provenance-footnote
  test('expands a calendar interaction to its linked meeting notes with provenance', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-005')
    const contactId = handleId(seeded, 'subject')
    await page.goto(`/contacts/${contactId}`)

    const interactions = page.getByRole('region', { name: 'Interactions' })
    const row = interactions.locator('[data-source="gcal"]')
    await expect(row.getByText('Meeting note', { exact: true })).toBeVisible()
    const interactionId = await row.getAttribute('data-interaction-id')
    expect(interactionId).toBeTruthy()
    await row.getByRole('button', { name: 'Expand content', exact: true }).click()

    const content = await fetchContent(request, interactionId!)
    const region = row.locator('[data-content-region]')
    await expect(region).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    const noteNodes = region.locator('[data-meeting-note]')
    await expect(noteNodes).toHaveCount(content.meeting_notes.length, { timeout: SLOW_TIMEOUT_MS })
    expect(content.meeting_notes).toHaveLength(2)
    expect(content.meeting_notes[0].title).toBe(handleName(seeded, 'note-a'))
    expect(content.meeting_notes[1].title).toBe(handleName(seeded, 'note-b'))

    for (let index = 0; index < content.meeting_notes.length; index += 1) {
      const note = content.meeting_notes[index]
      const node = noteNodes.nth(index)
      await expect(node.locator('[data-note-title]')).toHaveText(note.title!, {
        timeout: SLOW_TIMEOUT_MS,
      })
      await expect(node.locator('[data-note-summary]')).toHaveText(note.summary!, {
        timeout: SLOW_TIMEOUT_MS,
      })
      await expect(node.locator('[data-note-memo]')).toHaveText(note.memo!, {
        timeout: SLOW_TIMEOUT_MS,
      })
    }
    await expect(region.locator('i')).toHaveCount(0, { timeout: SLOW_TIMEOUT_MS })
    await expect(
      region.getByText('Meeting notes are processed on-device.', { exact: true })
    ).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
  })
})
