import { test, expect } from './fixtures'
import type { APIRequestContext } from '@playwright/test'
import { createTestAPI, TestAPI, declaredWorldNamePrefix } from './helpers/test-api'
import {
  candidateCardByName,
  expectModalCandidate,
  findCandidateByName,
  findCandidateByNameNoReload,
  resolverDialog,
} from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

/**
 * The nameless candidate's primary (only) address, read back from the
 * candidate endpoint. The generator owns it, so a test that restated the
 * address would be asserting against a string it invented.
 */
async function primaryEmailOf(request: APIRequestContext, candidateId: string): Promise<string> {
  const res = await request.get(`${API_BASE_URL}/api/v1/imports/${candidateId}`, {
    headers: API_HEADERS,
  })
  expect(res.ok()).toBe(true)
  const email: string = (await res.json())?.data?.emails?.[0]?.value
  expect(email, 'the declared nameless candidate must carry an address').toBeTruthy()
  return email
}

/**
 * The declared participant candidate's message_count, read back from the
 * API — the generator's message-count constant is internal, so a test
 * asserting an exact rendered count reads it back rather than hardcoding it.
 */
async function messageCountOf(request: APIRequestContext, candidateId: string): Promise<number> {
  const res = await request.get(`${API_BASE_URL}/api/v1/imports/${candidateId}`, {
    headers: API_HEADERS,
  })
  expect(res.ok()).toBe(true)
  const count: number = (await res.json())?.data?.metadata?.message_count
  expect(count, 'the declared participant candidate must carry a message count').toBeTruthy()
  return count
}

// gmail_participant's frontend surface (IMP-047, and the participant-source
// half of IMP-048's evidence line). Seeds through the declared behaviors
// EL3 registers: IMP-047 for the address-only ("nameless-participant")
// sighting, IMP-048 for the named, evidenced one ("participant" + "sender").
// There is no seedBehavior('IMP-042') — IMP-042 is surface: none and outside
// the declare universe; its citing tests are EL2's Go fakes and integration
// tests.
test.describe('Imports gmail_participant @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-047.identified-by-address-never-unknown, IMP-047.name-starts-empty, IMP-047.import-blocked-until-named
  test('nameless candidate is identified by address and importable after naming', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IMP-047')
    const candidateId = seeded.entities['nameless-participant'].id
    const address = await primaryEmailOf(request, candidateId)

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    // Located by an EXACT-match heading locator against the address: this
    // resolving at all is the proof the heading is the address rather than a
    // generic "Unknown" label (an exact match against a different string
    // would not have found it).
    await findCandidateByName(page, address)
    const card = candidateCardByName(page, address)
    await expect(card).toBeVisible({ timeout: 10000 })

    await card.getByRole('button', { name: /Import/i }).click()
    // Same exact-match proof, scoped to the modal heading this time.
    await expectModalCandidate(page, address)

    const modal = resolverDialog(page)
    const importButton = page.getByRole('button', { name: 'Import as New Contact', exact: true })
    await expect(importButton).toBeDisabled()

    // Enter edit mode: the editable name starts empty — never pre-filled
    // with the address, which is a display fallback only.
    await modal.getByRole('heading', { level: 3 }).first().click()
    const nameInput = modal.getByRole('textbox').first()
    await expect(nameInput).toBeVisible({ timeout: 5000 })
    await expect(nameInput).toHaveValue('')

    const typedName = `${declaredWorldNamePrefix(seeded)}Named After Import`
    await nameInput.fill(typedName)
    await nameInput.press('Enter')

    await expect(importButton).toBeEnabled()

    const importResponsePromise = page.waitForResponse(
      res =>
        res.url().includes(`/imports/${candidateId}/import`) && res.request().method() === 'POST'
    )
    await importButton.click()
    const importResponse = await importResponsePromise
    expect(importResponse.status()).toBe(201)
    const importBody = await importResponse.json()
    expect(importBody?.data?.contact?.full_name).toBe(typedName)

    // DOM precondition: the candidate card leaves the list.
    await expect(card).not.toBeVisible({ timeout: 15000 })

    // Persisted-state proof: the row is marked imported.
    const candidateRes = await request.get(`${API_BASE_URL}/api/v1/imports/${candidateId}`, {
      headers: API_HEADERS,
    })
    expect(candidateRes.ok()).toBe(true)
    expect((await candidateRes.json())?.data?.match_status).toBe('imported')
  })

  // spec: IMP-047.typed-name-survives-refresh
  test("a typed name survives the modal's background refetch", async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IMP-047')
    const candidateId = seeded.entities['nameless-participant'].id
    const address = await primaryEmailOf(request, candidateId)

    let releaseMountFetch = () => {}
    const mountFetchGate = new Promise<void>(resolve => {
      releaseMountFetch = resolve
    })
    let mountFetchIntercepted = false

    // The modal's own full-list query (page=1, limit=1000) fires its
    // deterministic mount refetch — a distinct query from the page's own
    // paginated queue fetch, matched here on the limit=1000 marker so only
    // the modal's request is held. Held via route.fetch()+fulfill() (not
    // route.continue()) because the released response must carry data that
    // actually re-fires the seeding effect. That effect's deps are
    // [mode, selectedContact, displayName, currentId, candidateHasSourceName]
    // — all derived primitives — so a metadata-only change (e.g. a bumped
    // message_count) touches NONE of them and the effect never re-runs,
    // which would make this test pass vacuously regardless of whether the
    // seed-key guard exists. The response instead adds a display_name for
    // the same candidate id — discovery later learning a name, a real
    // production shape — which flips candidateHasSourceName false→true and
    // changes displayName, genuinely re-firing the effect. The seed key
    // (`${currentId}:${mode}:${selectedContact?.id ?? 'none'}`) is
    // unchanged, so it is the GUARD, not the effect failing to re-run, that
    // must preserve the typed name.
    await page.route('**/api/v1/imports/candidates**', async route => {
      const url = route.request().url()
      if (route.request().method() !== 'GET' || !url.includes('limit=1000')) {
        return route.fallback()
      }
      mountFetchIntercepted = true
      await mountFetchGate
      const response = await route.fetch()
      const json = (await response.json()) as { data?: Array<Record<string, unknown>> }
      const candidates = json.data ?? []
      const target = candidates.find(c => c.id === candidateId)
      if (target) {
        // A name discovery learns for this address after the fact — the
        // exact case the modal's guard exists to survive.
        target.display_name = `${declaredWorldNamePrefix(seeded)}Discovered Later`
      }
      const headers = { ...response.headers() }
      delete headers['content-length']
      delete headers['content-encoding']
      await route.fulfill({
        status: response.status(),
        headers,
        contentType: 'application/json',
        body: JSON.stringify(json),
      })
    })

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    await findCandidateByName(page, address)
    const card = candidateCardByName(page, address)
    await card.getByRole('button', { name: /Import/i }).click()
    await expectModalCandidate(page, address)

    // The hold must actually be in effect before we type, or the race this
    // test exists to force never happens.
    await expect.poll(() => mountFetchIntercepted, { timeout: 5000 }).toBe(true)

    const modal = resolverDialog(page)
    await modal.getByRole('heading', { level: 3 }).first().click()
    const nameInput = modal.getByRole('textbox').first()
    await expect(nameInput).toBeVisible({ timeout: 5000 })
    await expect(nameInput).toHaveValue('')

    const typedName = `${declaredWorldNamePrefix(seeded)}Typed During Refetch`
    await nameInput.fill(typedName)

    // Release the held, CHANGED same-id response now that the input holds
    // the typed name mid-edit. The response promise is armed BEFORE
    // releasing so it cannot miss the fulfillment that follows.
    const releasedResponse = page.waitForResponse(
      res => res.url().includes('limit=1000') && res.request().method() === 'GET'
    )
    releaseMountFetch()
    await releasedResponse

    // The network round trip landing is not the same moment as React having
    // committed the re-render it triggers — the seeding effect fires (or, on
    // a clobber regression, reseeds) on the commit AFTER that. Without this
    // settle window, a same-tick assertion can observe the pre-render DOM and
    // pass vacuously whether or not the guard exists, because a same-tick
    // read observes the DOM from before the effect's commit.
    await page.waitForTimeout(500)

    // The typed name and edit mode must survive the refetch landing.
    await expect(nameInput).toHaveValue(typedName)
    await expect(
      page.getByRole('button', { name: 'Import as New Contact', exact: true })
    ).toBeEnabled()
  })

  // spec: IMP-048.count-recency-visible-any-source, IMP-048.counterpart-visible
  test('participant candidate renders trusted-sender evidence', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IMP-048')
    const participantId = seeded.entities['participant'].id
    const participantName = seeded.entities['participant'].name
    const senderName = seeded.entities['sender'].name
    const messageCount = await messageCountOf(request, participantId)

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    await findCandidateByName(page, participantName)
    const card = candidateCardByName(page, participantName)
    await expect(card).toBeVisible({ timeout: 10000 })
    await expect(card.getByText(`From ${senderName}`)).toBeVisible()
    await expect(
      card.getByText(`${messageCount} ${messageCount === 1 ? 'message' : 'messages'}`)
    ).toBeVisible()
    await expect(card.getByText(/Last: [A-Za-z]{3} \d{1,2}, \d{4}/)).toBeVisible()
  })

  test('the combined Gmail filter surfaces both Gmail sources', async ({ page }) => {
    const participantWorld = await testApi.seedBehavior('IMP-048')
    const participantName = participantWorld.entities['participant'].name
    const correspondenceWorld = await testApi.seedBehavior('IMP-037')
    const correspondenceName = correspondenceWorld.entities['corr'].name

    await page.goto('/imports')
    // The pill drives a server-side source-group filter; a fresh request
    // always fires because the filter changes the query key. Exclusion
    // semantics (non-gmail sources filtered out, single-source filters
    // unchanged) are pinned deterministically by the API integration test —
    // this test proves the UI wiring sends the COMBINED virtual value and
    // renders rows from BOTH member sources. The param is parsed exactly:
    // a substring check would also match a broken single-source value like
    // source=gmail_participant, passing while correspondence rows vanish.
    const filtered = page.waitForResponse(
      res =>
        res.request().method() === 'GET' &&
        new URL(res.url()).searchParams.get('source') === 'gmail'
    )
    await page.getByRole('button', { name: 'Gmail', exact: true }).click()
    await filtered
    await expect(page.getByRole('button', { name: 'Gmail', exact: true })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    // No-reload walks: the source filter is client state, and
    // findCandidateByName's reload recovery would silently reset it to All —
    // the walk would then "find" the cards under no filter at all. One card
    // from EACH Gmail source proves the group, not just a member.
    await findCandidateByNameNoReload(page, participantName)
    await findCandidateByNameNoReload(page, correspondenceName)
  })
})
