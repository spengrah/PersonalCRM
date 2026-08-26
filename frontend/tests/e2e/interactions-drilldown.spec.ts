import { test, expect, type APIRequestContext } from '@playwright/test'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

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

  // spec: IXN-004.full-plaintext-bodies, IXN-004.sender-and-timestamp, IXN-004.all-speakers-in-group, IXN-004.expand-in-place-no-nav
  test('expands a group thread to the full multi-speaker plaintext evidence', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-004')
    const contactId = handleId(seeded, 'subject')
    const interactionId = handleId(seeded, 'group-thread')
    await page.goto(`/contacts/${contactId}`)

    const interactions = page.getByRole('region', { name: 'Interactions' })
    const row = interactions.locator(`[data-interaction-id="${interactionId}"]`)
    await expect(row.getByText('2 messages', { exact: true })).toBeVisible()
    const urlBeforeExpand = page.url()
    await row.getByRole('button', { name: 'Expand content', exact: true }).click()

    const content = await fetchContent(request, interactionId)
    const region = row.locator('[data-content-region]')
    await expect(region).toBeVisible({ timeout: 15000 })
    const messageNodes = region.locator('[data-message-id]')
    await expect(messageNodes).toHaveCount(content.messages.length, { timeout: 15000 })
    await expect(
      messageNodes.evaluateAll(nodes => nodes.map(node => node.getAttribute('data-message-id')))
    ).resolves.toEqual(content.messages.map(message => message.id))

    for (let index = 0; index < content.messages.length; index += 1) {
      const message = content.messages[index]
      const node = messageNodes.nth(index)
      await expect(node.locator('[data-message-body]')).toHaveText(message.body, {
        timeout: 15000,
      })
      await expect(node.locator('[data-message-sender]')).toHaveText(message.sender, {
        timeout: 15000,
      })
      await expect(node.locator('[data-message-timestamp]')).toHaveText(
        expectedDateTime(message.sent_at),
        { timeout: 15000 }
      )
      if (message.body.includes('<b>not-markup</b>')) {
        await expect(region.locator('b')).toHaveCount(0, { timeout: 15000 })
      }
    }

    const senders = new Set(await region.locator('[data-message-sender]').allTextContents())
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
          timeout: 15000,
        }
      )
    }
    await expect(page).toHaveURL(urlBeforeExpand, { timeout: 15000 })
    await expect(page.getByRole('dialog')).toHaveCount(0, { timeout: 15000 })
    await expect(row).toBeVisible({ timeout: 15000 })

    await row.getByRole('button', { name: 'Collapse content', exact: true }).click()
    await expect(row.locator('[data-content-region]')).toHaveCount(0, { timeout: 15000 })
    await expect(row.getByRole('button', { name: 'Expand content', exact: true })).toBeVisible({
      timeout: 15000,
    })
    await expect(page).toHaveURL(urlBeforeExpand, { timeout: 15000 })
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
    await expect(region).toBeVisible({ timeout: 15000 })
    const noteNodes = region.locator('[data-meeting-note]')
    await expect(noteNodes).toHaveCount(content.meeting_notes.length, { timeout: 15000 })
    expect(content.meeting_notes).toHaveLength(2)
    expect(content.meeting_notes[0].title).toBe(handleName(seeded, 'note-a'))
    expect(content.meeting_notes[1].title).toBe(handleName(seeded, 'note-b'))

    for (let index = 0; index < content.meeting_notes.length; index += 1) {
      const note = content.meeting_notes[index]
      const node = noteNodes.nth(index)
      await expect(node.locator('[data-note-title]')).toHaveText(note.title!, {
        timeout: 15000,
      })
      await expect(node.locator('[data-note-summary]')).toHaveText(note.summary!, {
        timeout: 15000,
      })
      await expect(node.locator('[data-note-memo]')).toHaveText(note.memo!, {
        timeout: 15000,
      })
    }
    await expect(region.locator('i')).toHaveCount(0, { timeout: 15000 })
    await expect(
      region.getByText('Meeting notes are processed on-device.', { exact: true })
    ).toHaveCount(1, { timeout: 15000 })
  })
})
