import { test, expect, type Locator, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import { entity, fetchThreads, readThread, seedJudgmentRun, threadOf } from '../helpers/bench'

/** The Threads list's rows, in the order shown. */
function rows(page: Page): Locator {
  return page.getByRole('list', { name: 'Threads' }).locator(':scope > [role="listitem"]')
}

test.describe('Threads list @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    // The world seeds a chat for each of two pages of threads.
    test.slow()
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('a group chat of four contacts judged on twelve nights is one row naming all four', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-076')
    const people = ['ana', 'ben', 'cai', 'dee'].map(handle => entity(seeded, handle))
    // Ben speaks in the family chat and nowhere else.
    const family = await threadOf(request, people[1].id, undefined)
    const messages = (await readThread(request, family.thread_id)).messages
    expect(messages).toHaveLength(12)
    await seedJudgmentRun(request, {
      from: '2026-01-01T00:00:00Z',
      to: '2026-01-31T00:00:00Z',
      judgments: messages.map(message => ({
        thread_interaction_id: entity(seeded, 'family').id,
        cut_off: new Date(new Date(message.sent_at).getTime() + 30_000).toISOString(),
        window_start: messages[0].message_key,
        new_messages: [message.message_key],
      })),
    })

    await page.goto(`/interactions?tab=threads&contact=${people[1].id}`)

    // spec: IXN-076.one-row-per-thread, IXN-076.row-contents
    await expect(rows(page)).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
    const row = rows(page).first()
    await expect(row.locator('[data-group-chip]')).toHaveText('Group')
    const links = row.locator('[data-participants] a')
    await expect(links).toHaveCount(4)
    for (const person of people) {
      await expect(links.filter({ hasText: person.name })).toHaveAttribute(
        'href',
        `/contacts/${person.id}`
      )
    }
    await expect(row.locator('time')).toHaveAttribute('datetime', family.latest_at)
  })

  test('a row names a thread by its venue, title and latest message, and narrows by venue and contact', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-076')
    const [ana, host, solo] = ['ana', 'host', 'solo'].map(handle => entity(seeded, handle))
    const { data: anas } = await fetchThreads(request, { contact_id: ana.id })
    const mail = anas.items.find(thread => thread.venue === 'email')!
    const family = anas.items.find(thread => thread.is_group)!
    const { data: hosts } = await fetchThreads(request, { contact_id: host.id })
    const meeting = hosts.items[0]
    expect(family.title, 'a Telegram group is titled by its chat').toBeTruthy()
    expect(mail.title, 'an email thread has no title').toBeUndefined()

    await page.goto(`/interactions?tab=threads&contact=${ana.id}`)

    // spec: IXN-076.newest-message-first, IXN-076.row-contents
    await expect(rows(page)).toHaveCount(2, { timeout: SLOW_TIMEOUT_MS })
    expect(
      await rows(page).evaluateAll(nodes => nodes.map(node => node.getAttribute('data-thread-id')))
    ).toEqual([family.thread_id, mail.thread_id])
    const mailRow = rows(page).nth(1)
    await expect(mailRow.locator('[data-venue-chip]')).toHaveText('Email')
    await expect(mailRow.locator('[data-group-chip]')).toHaveCount(0)
    await expect(mailRow.locator('[data-thread-title]')).toHaveCount(0)
    const familyTitle = rows(page).first().locator('[data-thread-title]')
    await expect(familyTitle).toHaveText(family.title!)
    await expect(familyTitle).toHaveClass(/font-semibold/)
    await expect(mailRow.locator('[data-participants] a')).toHaveText([ana.name])
    await expect(mailRow.locator('time')).toHaveAttribute('datetime', mail.latest_at)

    // spec: IXN-076.message-count-review-off
    await expect(rows(page).first().locator('[data-count]')).toHaveText('12 messages')
    await expect(mailRow.locator('[data-count]')).toHaveText('1 message')

    // spec: IXN-076.venue-filter
    const venues = page.getByRole('group', { name: 'Venue' })
    await expect(venues.getByRole('button')).toHaveText([
      'All',
      'WhatsApp',
      'Telegram',
      'iMessage',
      'Email',
      'Calendar',
    ])
    await venues.getByRole('button', { name: 'Email' }).click()
    await expect(page).toHaveURL(/venue=email/)
    await expect(rows(page)).toHaveCount(1)
    await expect(rows(page).first()).toHaveAttribute('data-thread-id', mail.thread_id)
    await venues.getByRole('button', { name: 'Telegram' }).click()
    await expect(rows(page)).toHaveCount(1)
    await expect(rows(page).first()).toHaveAttribute('data-thread-id', family.thread_id)
    await venues.getByRole('button', { name: 'WhatsApp' }).click()
    await expect(page.getByText('No threads match these filters')).toBeVisible()

    // spec: IXN-076.row-contents, IXN-076.message-count-review-off
    await page.goto(`/interactions?tab=threads&contact=${host.id}&venue=calendar`)
    await expect(rows(page)).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
    await expect(rows(page).first()).toHaveAttribute('data-thread-id', meeting.thread_id)
    await expect(rows(page).first().locator('[data-venue-chip]')).toHaveText('Calendar')
    await expect(rows(page).first().locator('[data-count]')).toHaveText('1 meeting note')

    // spec: IXN-076.contact-filter, IXN-076.filters-combine-and-page
    await page.goto('/interactions?tab=threads')
    const contactFilter = page.getByRole('group', { name: 'Contact', exact: true })
    await contactFilter.getByText('Search contacts...').click({ timeout: SLOW_TIMEOUT_MS })
    await contactFilter.getByPlaceholder('Search contacts...').fill(solo.name)
    await contactFilter.getByRole('option').filter({ hasText: solo.name }).click()
    await expect(page).toHaveURL(new RegExp(`contact=${solo.id}`))
    const { data: solos } = await fetchThreads(request, { contact_id: solo.id })
    expect(solos.items).toHaveLength(1)
    await expect(rows(page)).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
    await expect(rows(page).first()).toHaveAttribute('data-thread-id', solos.items[0].thread_id)
    await page.getByRole('button', { name: 'Clear filters' }).click()
    await expect(page).toHaveURL(/\/interactions\?tab=threads$/)
  })

  test('pages as the Interactions tab pages, and a row opens its thread carrying the list filters', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-076')
    const many = entity(seeded, 'many')
    const { data, total } = await fetchThreads(request, { contact_id: many.id, limit: 100 })
    expect(total).toBe(21)
    const ids = data.items.map(thread => thread.thread_id)
    const shown = () =>
      rows(page).evaluateAll(nodes => nodes.map(node => node.getAttribute('data-thread-id')))

    // spec: IXN-076.filters-combine-and-page
    await page.goto(`/interactions?tab=threads&contact=${many.id}`)
    await expect(rows(page)).toHaveCount(20, { timeout: SLOW_TIMEOUT_MS })
    expect(await shown()).toEqual(ids.slice(0, 20))
    await expect(page.getByTestId('pagination')).toContainText('Page 1 of 2 (21 total threads)')
    await page.getByTestId('pagination').getByRole('button', { name: 'Next' }).click()
    await expect(page).toHaveURL(/page=2/)
    await expect(rows(page)).toHaveCount(1)
    expect(await shown()).toEqual(ids.slice(20))
    await page.reload()
    await expect(rows(page)).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
    expect(await shown()).toEqual(ids.slice(20))

    // spec: IXN-076.row-opens-thread-page
    await page.goto(`/interactions?tab=threads&venue=telegram&contact=${many.id}&page=2`)
    await expect(rows(page)).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
    await expect(rows(page).first().getByRole('link', { name: 'Open thread' })).toHaveAttribute(
      'href',
      `/interactions/threads/${ids[20]}?venue=telegram&contact=${many.id}&page=2`
    )
  })
})
