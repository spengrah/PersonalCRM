// This spec MUST import from '@playwright/test', never from './fixtures'.
//
// The custom fixture sets window.__PLAYWRIGHT__, which makes query-client.ts
// use staleTime: 0 (frontend/src/lib/query-client.ts). The behavior under test
// here IS the production five-minute cache — whether linking an import
// invalidates an already-cached contact detail entry. Under staleTime: 0 every
// remount refetches anyway, so the assertion would pass with or without the
// invalidation and this file would become decorative. A later "standardize the
// imports specs onto ./fixtures" edit silently deletes the coverage.
import { test, expect, Page } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import {
  candidateCardByName,
  expectModalCandidate,
  findCandidateByNameNoReload,
  resolverDialog,
} from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

/** Marker used to prove no full page load happened after it was planted. */
const SENTINEL = '__crmNoReloadSentinel'

type SentinelWindow = Window & { [SENTINEL]?: boolean }

async function plantNoReloadSentinel(page: Page): Promise<void> {
  await page.evaluate(name => {
    ;(window as SentinelWindow)[name as typeof SENTINEL] = true
  }, SENTINEL)
}

async function noReloadSentinelSurvived(page: Page): Promise<boolean> {
  return page.evaluate(
    name => (window as SentinelWindow)[name as typeof SENTINEL] === true,
    SENTINEL
  )
}

test.describe('Import link invalidates contact detail @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-040[0]
  test("linked candidate's new method appears on the contact detail without a refresh", async ({
    page,
    request,
  }) => {
    // This test walks four surfaces client-side (detail -> imports -> link ->
    // contacts -> detail) and cannot shortcut any of them with a goto, since a
    // hard navigation would discard the cache under test. The default 30s
    // budget is not enough for that route on the dev server.
    test.setTimeout(120_000)

    const prefix = testApi.prefix
    // Distinct values so the candidate does not auto-match the contact: the
    // link must be the explicit user action this spec drives.
    const seededEmail = `${prefix}-seeded@example.test`
    const candidateEmail = `${prefix}-linked@example.test`
    const contactName = `${prefix}-Invalidation Target`
    const candidateName = `${prefix}-Invalidation Candidate`

    const { ids: contactIds } = await testApi.seedContacts([
      {
        full_name: 'Invalidation Target',
        methods: [{ type: 'email', value: seededEmail, is_primary: true }],
      },
    ])
    const contactId = contactIds[0]

    // A second candidate keeps the review queue non-empty after the link. On a
    // clean E2E database the seeded candidate would otherwise be the last one,
    // and the resolver modal crashes its host page while unwinding an emptied
    // queue — a pre-existing defect unrelated to cache invalidation, which
    // would take the nav with it and strand the rest of this test.
    const { ids: externalIds } = await testApi.seedExternalContacts([
      { display_name: 'Invalidation Candidate', emails: [candidateEmail] },
      { display_name: 'Invalidation Bystander', emails: [`${prefix}-bystander@example.test`] },
    ])
    const externalId = externalIds[0]

    // --- Step 1: populate the contact detail cache -------------------------
    // The only hard navigation in this test. Everything after the sentinel is
    // planted must stay client-side.
    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    // The seeded method proves the detail query actually resolved and is now
    // cached — without this, "candidate email absent" would be trivially true
    // on a page that never loaded, and the cache under test would not exist.
    await expect(page.getByText(seededEmail)).toBeVisible({ timeout: 15000 })
    await expect(page.getByText(candidateEmail)).toHaveCount(0)

    await plantNoReloadSentinel(page)

    // --- Step 2: suppress rematch-watcher registration ---------------------
    // useLinkCandidate registers a RematchJobWatcher whenever the link response
    // carries a rematch_job_id, and that watcher independently invalidates
    // contactKeys.detail on job completion
    // (frontend/src/components/providers/rematch-job-watcher.tsx). Email is
    // rematch-eligible, so without this the detail cache would be invalidated
    // by that path regardless of the import:linked rule under test.
    //
    // Stripping the id from the response is what makes the import:linked rule
    // the ONLY thing that can invalidate the detail entry.
    const deliveredLinkBodies: Array<{ data?: { rematch_job_id?: string } }> = []

    await page.route(`**/api/v1/imports/${externalId}/link`, async route => {
      if (route.request().method() !== 'POST') {
        return route.fallback()
      }
      const response = await route.fetch()
      const json = (await response.json()) as { data?: { rematch_job_id?: string } }
      if (json?.data) {
        delete json.data.rematch_job_id
      }
      deliveredLinkBodies.push(json)
      // Upstream headers are forwarded so the cross-origin CORS headers survive,
      // but content-length/encoding are dropped: they describe the ORIGINAL
      // body, and re-serializing without rematch_job_id changes its length. A
      // stale content-length truncates the body and the app blows up parsing it.
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

    // --- Step 3: link the candidate, client-side throughout ----------------
    // Scoped to the app nav: these are next/link anchors, so the transition is
    // client-side and the query cache survives it.
    const nav = page.getByRole('navigation').first()
    await nav.getByRole('link', { name: 'Imports' }).click()
    await expect(page).toHaveURL(/\/imports/)

    // findCandidateByName is deliberately NOT used: it calls page.reload() on
    // retry attempts, which would destroy the very cache under test.
    await findCandidateByNameNoReload(page, candidateName)

    const card = candidateCardByName(page, candidateName)
    await card.getByRole('button', { name: /Link/i }).click()
    await expectModalCandidate(page, candidateName)

    const dialog = resolverDialog(page)
    await dialog.getByText('Search for a contact...').click()
    await dialog.getByPlaceholder('Search for a contact...').fill(prefix)
    const contactOption = dialog.getByText(contactName, { exact: false }).last()
    await expect(contactOption).toBeVisible({ timeout: 5000 })
    await contactOption.click()

    // The method comparison only renders once a target contact is chosen. The
    // candidate's email is offered there, pre-selected by default, so linking
    // applies it to the contact.
    await expect(dialog.getByText(candidateEmail)).toBeVisible({ timeout: 10000 })

    const linkResponsePromise = page.waitForResponse(
      res =>
        res.url().includes(`/api/v1/imports/${externalId}/link`) &&
        res.request().method() === 'POST'
    )
    await page.getByRole('button', { name: /Link Contact/i }).click()
    const linkResponse = await linkResponsePromise
    expect(linkResponse.ok()).toBe(true)

    // --- Step 4: prove the watcher suppression actually happened -----------
    // A page.route that silently fails to match leaves the watcher registered
    // and this whole test vacuous, so the interception is asserted rather than
    // assumed.
    expect(deliveredLinkBodies.length, 'link response interception must have fired').toBe(1)
    expect(
      deliveredLinkBodies[0]?.data,
      'intercepted response body should have been delivered'
    ).toBeTruthy()
    expect(
      deliveredLinkBodies[0]?.data?.rematch_job_id,
      'the response the app received must carry no rematch_job_id'
    ).toBeUndefined()

    // Server-side proof that the method landed. This separates "the link did
    // not apply the email" from "the UI did not refresh", so a failure below is
    // unambiguously about the cache.
    const contactRes = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
      headers: API_HEADERS,
    })
    expect(contactRes.ok()).toBe(true)
    const contactBody = await contactRes.json()
    const methods: Array<{ type: string; value: string }> = contactBody?.data?.methods ?? []
    expect(
      methods.some(m => m.type === 'email' && m.value === candidateEmail),
      'linking should have added the candidate email server-side'
    ).toBe(true)

    // A render crash replaces the whole app with the error boundary, which
    // takes the nav with it and turns every later step into an unexplained
    // locator timeout. Assert it directly so a crash names itself.
    await expect(page.getByRole('heading', { name: 'Something went wrong' })).toHaveCount(0)

    // --- Step 5: navigate back to the contact, still client-side -----------
    await page.keyboard.press('Escape')
    await expect(dialog).toBeHidden({ timeout: 10000 })

    await nav.getByRole('link', { name: 'Contacts' }).click()
    await expect(page).toHaveURL(/\/contacts/)
    await page.getByPlaceholder('Search contacts...').fill(prefix)
    const contactLink = page.getByRole('link', { name: contactName, exact: true })
    await expect(contactLink).toBeVisible({ timeout: 15000 })
    await contactLink.click()

    // --- Step 6: the assertion this spec exists for ------------------------
    // Without the import:linked detail invalidation, the detail entry cached in
    // step 1 is still fresh (staleTime is five minutes in production) and the
    // remount serves it, so the newly linked email never appears.
    await expect(page.getByText(candidateEmail)).toBeVisible({ timeout: 15000 })

    // --- Step 7: prove the whole run was reload-free ------------------------
    // A full page load from ANY source — a stray goto, a helper's reload, an
    // app-level window.location — wipes the query cache and makes step 6 pass
    // regardless of the fix. The sentinel turns that into a loud failure.
    expect(
      await noReloadSentinelSurvived(page),
      'a full page load occurred, which discards the query cache and makes this test vacuous'
    ).toBe(true)
  })
})
