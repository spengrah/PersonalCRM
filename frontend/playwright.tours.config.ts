import { defineConfig, devices } from '@playwright/test'

// Dedicated, ISOLATED tours config. This file is NEVER imported
// by playwright.config.ts and is only ever reached via an explicit
// `--config=playwright.tours.config.ts` (make tours / scripts/run-tours.sh).
//
// Isolation is safety-critical: a tours sweep runs staging-reset.sh, which
// HARD-WIPES staging. A bare `bunx playwright test` uses the default config
// (tests/e2e only) and can never reach a tour. Its own testDir/testMatch, no
// webServer (tours target an already-running remote staging app), a single
// serial chromium project, and no retries keep an accidental staging wipe
// impossible.
export default defineConfig({
  testDir: './tests/tours',
  testMatch: '**/*.tour.ts',
  fullyParallel: false,
  workers: 1,
  // A tour error is signal (navigation breakage / missing control), not flake —
  // never paper over it with a retry.
  retries: 0,
  reporter: [['list']],
  globalSetup: './tests/tours/support/global-setup.ts',
  projects: [
    {
      name: 'tours',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  use: {
    // Staging frontend — NO default (globalSetup fails loudly if unset).
    baseURL: process.env.TOURS_BASE_URL,
    trace: 'on',
    screenshot: 'only-on-failure',
    // Pin the browser's calendar zone + locale to UTC so "today" resolves in the
    // same zone the reseed computes the clock-anchored birthday fixtures in
    // (staging-reset.sh pins the reseed container to TZ=UTC). Without this the
    // judge host's local zone could shift the browser's day off the seed's, so an
    // imminent fixture might not land in the birthdays page's highlight window.
    timezoneId: 'UTC',
    locale: 'en-US',
  },
  // NO webServer — tours target a remote, already-running staging app.
})
