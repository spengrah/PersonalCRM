import { test, expect, type Page } from '@playwright/test'
import { fulfillJson } from './helpers/fulfill-json'

// The WhatsApp section talks to the app's own /api/v1/whatsapp/* endpoints,
// which cannot run live in E2E: whatsmeow needs a real linked phone, and no
// test can scan a QR code with one. Every test here route-mocks that boundary —
// the sanctioned technique — and asserts the real surface branches the mocked
// states drive. The contract on the other side of the mock is separately pinned
// by the Go API tests over the real router.

type Json = Record<string, unknown>

// statusPayload builds a full status body. backfill and ingest are NOT optional
// on the wire, and the section reads into both, so every mock carries them.
function statusPayload(overrides: Json = {}): Json {
  return {
    configured: true,
    state: 'not_paired',
    backfill: { pending: 0, processing: 0, failed: 0, dropped_inline_chunks: 0 },
    ingest: { unresolved_lid_peers: 0 },
    ...overrides,
  }
}

async function mockStatus(page: Page, data: Json) {
  await page.route('**/api/v1/whatsapp/auth/status', route =>
    fulfillJson(route, { success: true, data })
  )
}

// mockChats stands in for the chat list the connected branch mounts. Tests that
// are not about chats still need it, or the list would fall through to the live
// backend (feature off ⇒ 404) and render its error branch.
async function mockChats(page: Page, chats: Json[]) {
  await page.route('**/api/v1/whatsapp/chats', route =>
    fulfillJson(route, { success: true, data: chats })
  )
}

const whatsappRegion = (page: Page) => page.getByRole('region', { name: 'WhatsApp' })

async function openSettings(page: Page) {
  await page.goto('/settings')
  await page.waitForLoadState('domcontentloaded')
}

test.describe('WhatsApp Settings @area:settings', () => {
  // spec: SET-036.section-present-settings-surface
  test('shows the WhatsApp section on the settings page', async ({ page }) => {
    // No mock at all: the E2E backend runs with the feature off, so this is the
    // real page rendering the real section.
    await openSettings(page)

    await expect(page.getByRole('heading', { name: 'WhatsApp', exact: true })).toBeVisible({
      timeout: 10000,
    })
    await expect(whatsappRegion(page)).toBeVisible()
  })

  // spec: SET-036.not-configured-state-when-disabled
  test('a not-configured backend names the configuration required', async ({ page }) => {
    // The section keys this off a 404: the WhatsApp routes are not registered
    // at all when the feature is off, so gin's own 404 is the signal.
    await page.route('**/api/v1/whatsapp/auth/status', route =>
      fulfillJson(
        route,
        { success: false, error: { code: 'NOT_FOUND', message: 'not found' } },
        404
      )
    )

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText('Configuration Required')).toBeVisible({ timeout: 10000 })
    // Both variables are named because the backend refuses to start with
    // WhatsApp enabled while external sync is off — naming only the first would
    // send the user into a failed start.
    await expect(section.getByText(/ENABLE_WHATSAPP_SYNC/)).toBeVisible()
    await expect(section.getByText(/ENABLE_EXTERNAL_SYNC/)).toBeVisible()
  })

  test('a failed status load offers a retry', async ({ page }) => {
    await page.route('**/api/v1/whatsapp/auth/status', route =>
      fulfillJson(route, { success: false, error: { code: 'INTERNAL', message: 'boom' } }, 500)
    )

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText('Failed to load WhatsApp status')).toBeVisible({
      timeout: 10000,
    })
    await expect(section.getByRole('button', { name: 'Retry' })).toBeVisible()
  })

  test('the section reports that it is still loading', async ({ page }) => {
    await page.route('**/api/v1/whatsapp/auth/status', async route => {
      await new Promise(resolve => setTimeout(resolve, 3000))
      return fulfillJson(route, { success: true, data: statusPayload() })
    })

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByRole('status', { name: 'Loading WhatsApp status' })).toBeVisible({
      timeout: 10000,
    })
    // And it resolves rather than spinning forever.
    await expect(section.getByRole('button', { name: 'Link WhatsApp', exact: true })).toBeVisible({
      timeout: 10000,
    })
  })

  // spec: WHA-071.not-ready-names-the-missing-piece
  // spec: WHA-071.not-ready-offers-no-link-affordance
  test('a not-ready integration names what it is waiting on and offers no link', async ({
    page,
  }) => {
    await mockStatus(
      page,
      statusPayload({ state: 'not_ready', missing: 'history drain worker is not registered' })
    )

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText(/WhatsApp cannot link an account yet/)).toBeVisible({
      timeout: 10000,
    })
    await expect(section.getByText(/history drain worker is not registered/)).toBeVisible()
    // The button is ABSENT, not disabled: the backend would answer 409, and a
    // disabled control invites the user to wonder what unlocks it.
    await expect(section.getByRole('button', { name: 'Link WhatsApp', exact: true })).toHaveCount(0)
  })

  // spec: WHA-070.link-offers-qr-and-phone
  test('linking offers both a QR code and a phone pairing code', async ({ page }) => {
    await mockStatus(page, statusPayload({ state: 'not_paired' }))

    await openSettings(page)

    const section = whatsappRegion(page)
    await section
      .getByRole('button', { name: 'Link WhatsApp', exact: true })
      .click({ timeout: 10000 })
    await expect(section.getByRole('button', { name: 'Scan a QR code' })).toBeVisible()
    await expect(section.getByRole('button', { name: 'Use a phone pairing code' })).toBeVisible()
  })

  // spec: WHA-070.qr-pairing-shows-scannable-code
  test('QR pairing renders the scannable code, and a refreshed code replaces it', async ({
    page,
  }) => {
    // Codes expire, so the section polls; what the E2E can prove is the
    // mechanism — the rendered code follows the polled status.
    // The first TWO polls serve the same code: the 3s refetch would otherwise
    // race the first assertion, and a swap that happens before it lands would
    // read as the code never having arrived.
    let poll = 0
    await page.route('**/api/v1/whatsapp/auth/status', route => {
      poll += 1
      return fulfillJson(route, {
        success: true,
        data: statusPayload({
          state: 'pairing',
          pairing: {
            method: 'qr',
            qr_code: poll <= 2 ? 'MOCK-QR-FIRST' : 'MOCK-QR-REFRESHED',
            expires_at: '2030-01-01T00:00:00Z',
          },
        }),
      })
    })

    await openSettings(page)

    const section = whatsappRegion(page)
    const figure = section.getByRole('img', { name: 'WhatsApp pairing QR code' })
    await expect(figure).toBeVisible({ timeout: 10000 })
    // The accessible name is the human description; the code itself is exposed
    // on a value attribute, so this asserts the MOCKED code reached the DOM
    // rather than that some image exists.
    await expect(figure).toHaveAttribute('data-qr-value', 'MOCK-QR-FIRST')
    await expect(figure).toHaveAttribute('data-qr-value', 'MOCK-QR-REFRESHED', { timeout: 15000 })
  })

  // spec: WHA-070.phone-pairing-shows-typed-code
  test('phone pairing sends the number and renders the code to type', async ({ page }) => {
    // One handler serves the mutating call AND the status poll that reflects
    // it. The glob is `auth**`, never `auth/**`: DELETE /whatsapp/auth has no
    // trailing segment, so a /** form would not match it, and the same shape is
    // used everywhere here for consistency.
    let pairing = false
    await page.route('**/api/v1/whatsapp/auth**', route => {
      const req = route.request()
      const path = new URL(req.url()).pathname
      if (req.method() === 'POST' && path.endsWith('/auth/start')) {
        pairing = true
        return fulfillJson(route, { success: true, data: statusPayload() }, 202)
      }
      if (req.method() === 'GET' && path.endsWith('/auth/status')) {
        return fulfillJson(route, {
          success: true,
          data: pairing
            ? statusPayload({
                state: 'pairing',
                pairing: {
                  method: 'phone',
                  pair_code: 'ABCD-1234',
                  expires_at: '2030-01-01T00:00:00Z',
                },
              })
            : statusPayload({ state: 'not_paired' }),
        })
      }
      return route.fallback()
    })

    await openSettings(page)

    const section = whatsappRegion(page)
    await section
      .getByRole('button', { name: 'Link WhatsApp', exact: true })
      .click({ timeout: 10000 })
    await section.getByRole('button', { name: 'Use a phone pairing code' }).click()
    await section.getByLabel('Phone Number').fill('+15551234567')

    // Arm the wait BEFORE the click: the DOM alone does not prove the wiring.
    const startRequest = page.waitForRequest(
      req => req.method() === 'POST' && req.url().includes('/api/v1/whatsapp/auth/start')
    )
    await section.getByRole('button', { name: 'Send Pairing Code' }).click()
    const posted = await startRequest
    expect(posted.postDataJSON()).toMatchObject({ method: 'phone', phone: '+15551234567' })

    await expect(section.getByText('ABCD-1234')).toBeVisible({ timeout: 10000 })
  })

  // spec: WHA-070.pairing-can-be-cancelled
  test('a pairing can be cancelled, returning the section to its link affordance', async ({
    page,
  }) => {
    let cancelled = false
    await page.route('**/api/v1/whatsapp/auth**', route => {
      const req = route.request()
      const path = new URL(req.url()).pathname
      if (req.method() === 'POST' && path.endsWith('/auth/cancel')) {
        cancelled = true
        return route.fulfill({ status: 204, body: '' })
      }
      if (req.method() === 'GET' && path.endsWith('/auth/status')) {
        return fulfillJson(route, {
          success: true,
          data: cancelled
            ? statusPayload({ state: 'not_paired' })
            : statusPayload({
                state: 'pairing',
                pairing: {
                  method: 'qr',
                  qr_code: 'MOCK-QR-CANCEL',
                  expires_at: '2030-01-01T00:00:00Z',
                },
              }),
        })
      }
      return route.fallback()
    })

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByRole('img', { name: 'WhatsApp pairing QR code' })).toBeVisible({
      timeout: 10000,
    })
    await section.getByRole('button', { name: 'Cancel' }).click()

    // The Link button — not the method chooser — is what must come back. That
    // assertion is what keeps the pair-mode reset rule from being decorative: a
    // cancel that left the mode set would drop the user back into the middle of
    // the flow they just abandoned.
    await expect(section.getByRole('button', { name: 'Link WhatsApp', exact: true })).toBeVisible({
      timeout: 10000,
    })
    await expect(section.getByRole('button', { name: 'Scan a QR code' })).toHaveCount(0)
  })

  // spec: WHA-072.account-identity-shown
  test('the connected section reports the linked account', async ({ page }) => {
    await mockStatus(
      page,
      statusPayload({
        state: 'connected',
        push_name: 'Test Account',
        phone_number: '+15559876543',
        jid: '15559876543:12@s.whatsapp.net',
        connected_at: '2026-08-01T09:00:00Z',
      })
    )
    await mockChats(page, [])

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText(/Connected as Test Account/)).toBeVisible({ timeout: 10000 })
    await expect(section.getByText(/\+15559876543/)).toBeVisible()
    await expect(section.getByText(/15559876543:12@s\.whatsapp\.net/)).toBeVisible()
    await expect(section.getByText(/Connected since/)).toBeVisible()
  })

  // spec: WHA-072.history-import-progress-shown
  // spec: WHA-072.history-floor-shown-once-known
  test('history import progress and its floor are reported', async ({ page }) => {
    await mockStatus(
      page,
      statusPayload({
        state: 'connected',
        backfill: {
          pending: 2,
          processing: 1,
          failed: 1,
          dropped_inline_chunks: 0,
          observed_floor_at: '2024-03-05T00:00:00Z',
        },
      })
    )
    await mockChats(page, [])

    await openSettings(page)

    const section = whatsappRegion(page)
    // 2 pending + 1 processing = 3 chunks still to import.
    await expect(section.getByText(/Importing message history… 3 chunks remaining/)).toBeVisible({
      timeout: 10000,
    })
    await expect(section.getByText(/1 chunk\(s\) could not be imported/)).toBeVisible()
    await expect(section.getByText(/History reaches back to/)).toBeVisible()
  })

  // spec: WHA-072.unresolved-peers-count-shown
  // spec: WHA-072.unrefreshed-counts-are-marked-stale
  test('unidentified peers are counted and unrefreshed counts are marked stale', async ({
    page,
  }) => {
    await mockStatus(
      page,
      statusPayload({
        state: 'connected',
        backfill: { pending: 0, processing: 0, failed: 0, dropped_inline_chunks: 0, stale: true },
        ingest: { unresolved_lid_peers: 4 },
      })
    )
    await mockChats(page, [])

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText(/Unidentified peers observed: 4/)).toBeVisible({
      timeout: 10000,
    })
    // A stale count is labelled rather than presented as a fresh zero.
    await expect(section.getByText(/Counts may be out of date/)).toBeVisible()
  })

  // spec: WHA-073.dropped-history-warned-when-present
  // spec: WHA-073.dropped-history-named-unrecoverable-without-relink
  test('a dropped chunk of one-shot history is warned about and named unrecoverable', async ({
    page,
  }) => {
    await mockStatus(
      page,
      statusPayload({
        state: 'connected',
        backfill: { pending: 0, processing: 0, failed: 0, dropped_inline_chunks: 2 },
      })
    )
    await mockChats(page, [])

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText(/2 chunk\(s\) of message history were dropped/)).toBeVisible({
      timeout: 10000,
    })
    await expect(
      section.getByText(/not recoverable without unlinking and pairing again/)
    ).toBeVisible()
  })

  // spec: WHA-073.dropped-history-warning-absent-when-none
  test('no dropped-history warning appears when nothing was dropped', async ({ page }) => {
    // The negative control for the test above: without it, a warning rendered
    // unconditionally would look exactly as correct.
    await mockStatus(
      page,
      statusPayload({
        state: 'connected',
        push_name: 'No Gap',
        backfill: { pending: 0, processing: 0, failed: 0, dropped_inline_chunks: 0 },
      })
    )
    await mockChats(page, [])

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText(/Connected as No Gap/)).toBeVisible({ timeout: 10000 })
    await expect(section.getByText(/of message history were dropped/)).toHaveCount(0)
  })

  // spec: WHA-074.disconnect-offered-and-confirmed
  test('disconnect is confirmed and returns the section to not-linked', async ({ page }) => {
    let disconnected = false
    await page.route('**/api/v1/whatsapp/auth**', route => {
      const req = route.request()
      const path = new URL(req.url()).pathname
      if (req.method() === 'DELETE' && path.endsWith('/whatsapp/auth')) {
        disconnected = true
        return fulfillJson(route, {
          success: true,
          data: { remote_unlinked: true, already_unlinked: false, forced: false },
        })
      }
      if (req.method() === 'GET' && path.endsWith('/auth/status')) {
        return fulfillJson(route, {
          success: true,
          data: disconnected
            ? statusPayload({ state: 'not_paired' })
            : statusPayload({ state: 'connected', push_name: 'Unlink Me' }),
        })
      }
      return route.fallback()
    })
    await mockChats(page, [])

    // The disconnect affordance confirms via a native dialog.
    page.on('dialog', dialog => dialog.accept())

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText(/Connected as Unlink Me/)).toBeVisible({ timeout: 10000 })

    const deleteRequest = page.waitForRequest(
      req => req.method() === 'DELETE' && req.url().includes('/api/v1/whatsapp/auth')
    )
    await section.getByRole('button', { name: /Disconnect/ }).click()
    await deleteRequest

    await expect(section.getByRole('button', { name: 'Link WhatsApp', exact: true })).toBeVisible({
      timeout: 10000,
    })
  })

  // spec: WHA-074.failed-unlink-keeps-credentials-and-offers-retry
  test('a failed unlink says the credentials were kept and offers a retry', async ({ page }) => {
    await mockStatus(
      page,
      statusPayload({ state: 'disconnect_failed', reason: 'local_cleanup_failed' })
    )

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText(/The unlink did not complete/)).toBeVisible({ timeout: 10000 })
    await expect(section.getByText(/credentials were deliberately\s+kept/)).toBeVisible()
    await expect(section.getByRole('button', { name: 'Retry disconnect' })).toBeVisible()
  })

  // spec: WHA-074.forced-clear-warns-device-must-be-unlinked-from-phone
  test('a forced clear warns that the device must be unlinked from the phone', async ({ page }) => {
    await page.route('**/api/v1/whatsapp/auth**', route => {
      const req = route.request()
      const path = new URL(req.url()).pathname
      if (req.method() === 'DELETE' && path.endsWith('/whatsapp/auth')) {
        return fulfillJson(route, {
          success: true,
          data: {
            remote_unlinked: false,
            already_unlinked: false,
            forced: true,
            warning:
              'Credentials cleared locally. Remove this device from your phone: WhatsApp > Settings > Linked Devices.',
          },
        })
      }
      if (req.method() === 'GET' && path.endsWith('/auth/status')) {
        return fulfillJson(route, {
          success: true,
          data: statusPayload({ state: 'disconnect_failed', reason: 'local_cleanup_failed' }),
        })
      }
      return route.fallback()
    })

    page.on('dialog', dialog => dialog.accept())

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByRole('button', { name: 'Force clear' })).toBeVisible({
      timeout: 10000,
    })
    // The standing copy states the consequence before the user commits.
    await expect(section.getByText(/remove this device from\s+your phone/)).toBeVisible()

    const forcedRequest = page.waitForRequest(
      req => req.method() === 'DELETE' && req.url().includes('force=true')
    )
    await section.getByRole('button', { name: 'Force clear' }).click()
    await forcedRequest

    await expect(section.getByText(/Remove this device from your phone/)).toBeVisible({
      timeout: 10000,
    })
  })

  // spec: WHA-075.degraded-store-surfaced
  // spec: WHA-075.remedy-is-force-disconnect-and-relink
  test('a degraded device store is surfaced with its remedy', async ({ page }) => {
    const cases: Array<{ flags: Json; note: RegExp }> = [
      { flags: { replaced_device_retained: true }, note: /previous device could not be removed/ },
      {
        flags: { terminal_reason_persisted: false },
        note: /reason this connection ended could not be recorded/,
      },
      {
        flags: { link_selector_persisted: false },
        note: /record of which device is linked could not be written/,
      },
    ]

    for (const { flags, note } of cases) {
      await page.unrouteAll({ behavior: 'ignoreErrors' })
      await mockStatus(page, statusPayload({ state: 'connected', ...flags }))
      await mockChats(page, [])

      await openSettings(page)

      const section = whatsappRegion(page)
      await expect(section.getByText(/stored WhatsApp device state is degraded/)).toBeVisible({
        timeout: 10000,
      })
      await expect(section.getByText(note)).toBeVisible()
      // One remedy, named in the words the backend documents.
      await expect(section.getByText(/Force disconnect clears it/)).toBeVisible()
    }
  })

  // spec: WHA-076.groups-listed-with-size-and-decision
  test('discovered groups are listed with their size and tracking decision', async ({ page }) => {
    await mockStatus(page, statusPayload({ state: 'connected' }))
    await mockChats(page, [
      {
        chat_jid: '111-1@g.us',
        chat_title: 'Book Club',
        chat_type: 'group',
        member_count: 4,
        status: 'auto',
        effective_tracked: true,
      },
      {
        chat_jid: '222-2@g.us',
        chat_title: 'Big Group',
        chat_type: 'group',
        member_count: 400,
        status: 'ignored',
        effective_tracked: false,
      },
      {
        chat_jid: '333-3@g.us',
        chat_title: 'Unsized Group',
        chat_type: 'group',
        status: 'auto',
        effective_tracked: false,
      },
    ])

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText('Book Club')).toBeVisible({ timeout: 10000 })
    await expect(section.getByText('4 members')).toBeVisible()
    await expect(section.getByText('400 members')).toBeVisible()
    // A group whose size WhatsApp never reported says so, rather than showing a
    // zero that would read as an empty group.
    await expect(section.getByText('size unknown')).toBeVisible()

    // The stored decision is what each row's control shows.
    await expect(section.getByLabel('Tracking for Book Club')).toHaveValue('auto')
    await expect(section.getByLabel('Tracking for Big Group')).toHaveValue('ignored')
  })

  // spec: WHA-076.group-without-a-title-is-identified-by-its-address
  test('a group with no title is identified by its address', async ({ page }) => {
    await mockStatus(page, statusPayload({ state: 'connected' }))
    await mockChats(page, [
      {
        chat_jid: '999-9@g.us',
        chat_type: 'group',
        member_count: 6,
        status: 'auto',
        effective_tracked: true,
      },
    ])

    await openSettings(page)

    const section = whatsappRegion(page)
    // The JID, not a placeholder: two unnamed groups must stay distinguishable.
    await expect(section.getByText('999-9@g.us')).toBeVisible({ timeout: 10000 })
    await expect(section.getByLabel('Tracking for 999-9@g.us')).toBeVisible()
  })

  // spec: WHA-076.tracking-choice-persists
  test('a tracking choice is recorded and reflected on the next read', async ({ page }) => {
    let status: 'auto' | 'tracked' | 'ignored' = 'auto'
    await mockStatus(page, statusPayload({ state: 'connected' }))
    await page.route('**/api/v1/whatsapp/chats**', route => {
      const req = route.request()
      if (req.method() === 'PATCH') {
        const body = req.postDataJSON() as { status: typeof status }
        status = body.status
        return fulfillJson(route, {
          success: true,
          data: {
            chat_jid: '111-1@g.us',
            chat_title: 'Book Club',
            chat_type: 'group',
            member_count: 400,
            status,
            effective_tracked: status === 'tracked',
          },
        })
      }
      return fulfillJson(route, {
        success: true,
        data: [
          {
            chat_jid: '111-1@g.us',
            chat_title: 'Book Club',
            chat_type: 'group',
            member_count: 400,
            status,
            effective_tracked: status === 'tracked',
          },
        ],
      })
    })

    await openSettings(page)

    const section = whatsappRegion(page)
    const select = section.getByLabel('Tracking for Book Club')
    await expect(select).toHaveValue('auto', { timeout: 10000 })

    const patchRequest = page.waitForRequest(
      req => req.method() === 'PATCH' && req.url().includes('/api/v1/whatsapp/chats/')
    )
    await select.selectOption('tracked')
    const patched = await patchRequest
    expect(patched.postDataJSON()).toMatchObject({ status: 'tracked' })
    // The JID travels in the path, percent-encoded.
    expect(decodeURIComponent(new URL(patched.url()).pathname)).toContain('111-1@g.us')

    // The refetch reflects the stored choice, so the change survives the read.
    await expect(select).toHaveValue('tracked', { timeout: 10000 })
  })

  // spec: WHA-076.untracked-history-is-not-recoverable
  test('the chat list says untracked history cannot be recovered', async ({ page }) => {
    await mockStatus(page, statusPayload({ state: 'connected' }))
    await mockChats(page, [
      {
        chat_jid: '111-1@g.us',
        chat_title: 'Book Club',
        chat_type: 'group',
        member_count: 4,
        status: 'auto',
        effective_tracked: true,
      },
    ])

    await openSettings(page)

    const section = whatsappRegion(page)
    // Honest copy rather than a retroactive backfill the source cannot do:
    // WhatsApp history arrives once, at link time.
    await expect(
      section.getByText(/messages received while it was\s+untracked were never stored/)
    ).toBeVisible({ timeout: 10000 })
  })

  // spec: WHA-076.empty-state-before-any-group
  test('the chat list shows an empty state before any group is discovered', async ({ page }) => {
    await mockStatus(page, statusPayload({ state: 'connected' }))
    await mockChats(page, [])

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText(/No group chats discovered yet/)).toBeVisible({ timeout: 10000 })
  })

  test('connecting and reconnecting both render a waiting state', async ({ page }) => {
    await mockStatus(page, statusPayload({ state: 'connecting' }))
    await openSettings(page)
    await expect(whatsappRegion(page).getByText('Connecting to WhatsApp…')).toBeVisible({
      timeout: 10000,
    })

    await page.unrouteAll({ behavior: 'ignoreErrors' })
    await mockStatus(page, statusPayload({ state: 'reconnecting' }))
    await openSettings(page)
    await expect(whatsappRegion(page).getByText('Reconnecting to WhatsApp…')).toBeVisible({
      timeout: 10000,
    })
  })

  // spec: WHA-077.disconnected-shows-the-reason
  // spec: WHA-077.temporary-ban-shows-when-it-lifts
  test('a lost connection shows its reason, and a temporary ban shows when it lifts', async ({
    page,
  }) => {
    await mockStatus(page, statusPayload({ state: 'disconnected', reason: 'logged_out' }))
    await openSettings(page)

    let section = whatsappRegion(page)
    await expect(section.getByText(/WhatsApp is disconnected/)).toBeVisible({ timeout: 10000 })
    // The machine reason is rendered in human words.
    await expect(section.getByText(/device was unlinked from WhatsApp/)).toBeVisible()

    await page.unrouteAll({ behavior: 'ignoreErrors' })
    await mockStatus(
      page,
      statusPayload({
        state: 'disconnected',
        reason: 'temporary_ban',
        banned_until: '2026-09-01T10:00:00Z',
      })
    )
    await openSettings(page)

    section = whatsappRegion(page)
    await expect(section.getByText(/temporarily restricted this account/)).toBeVisible({
      timeout: 10000,
    })
    // The lift time is the only actionable half of a ban.
    await expect(section.getByText(/The restriction lifts/)).toBeVisible()
  })

  // spec: WHA-077.startup-failure-shows-its-reason
  test('a startup failure shows its reason', async ({ page }) => {
    await mockStatus(page, statusPayload({ state: 'error', reason: 'device_store_ambiguous' }))

    await openSettings(page)

    const section = whatsappRegion(page)
    await expect(section.getByText(/WhatsApp could not start/)).toBeVisible({ timeout: 10000 })
    await expect(section.getByText(/could not be resolved to the linked account/)).toBeVisible()
  })

  // spec: WHA-077.terminal-disconnect-offers-a-fresh-link
  test('a terminal disconnect offers a fresh link that reaches the method choice', async ({
    page,
  }) => {
    // A terminal reason means the integration will not reconnect on its own, so
    // linking again is the only way forward. The affordance is only real if it
    // reaches the SAME method choice a first-time link does — the step is
    // derived from the backend state, which still reads disconnected.
    await mockStatus(page, statusPayload({ state: 'disconnected', reason: 'logged_out' }))

    await openSettings(page)

    const section = whatsappRegion(page)
    const relink = section.getByRole('button', { name: 'Link WhatsApp again' })
    await expect(relink).toBeVisible({ timeout: 10000 })
    await relink.click()

    await expect(section.getByRole('button', { name: 'Scan a QR code' })).toBeVisible()
    await expect(section.getByRole('button', { name: 'Use a phone pairing code' })).toBeVisible()
  })
})
