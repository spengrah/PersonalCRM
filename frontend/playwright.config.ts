import { defineConfig, devices } from '@playwright/test'
import os from 'os'

const frontendPort = process.env.E2E_FRONTEND_PORT || '3000'
const backendPort = process.env.E2E_BACKEND_PORT || '8080'
const frontendURL = `http://localhost:${frontendPort}`
const backendURL = `http://localhost:${backendPort}`

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  // Allow safe parallelism; override with PLAYWRIGHT_WORKERS if needed
  // Pi (arm64) runs faster with 1 worker due to resource contention
  workers: (() => {
    const configured = Number.parseInt(process.env.PLAYWRIGHT_WORKERS || '', 10)
    if (Number.isFinite(configured) && configured > 0) {
      return configured
    }

    // Raspberry Pi (Linux arm64): 1 worker is faster than parallel due to memory/CPU contention
    // Apple Silicon Macs (darwin arm64) are fast enough for parallel
    if (os.arch() === 'arm64' && os.platform() === 'linux') {
      return 1
    }

    const cpuCount = os.cpus().length || 1
    const maxWorkers = cpuCount >= 8 ? 4 : cpuCount >= 4 ? 3 : 2
    return Math.max(1, Math.min(maxWorkers, cpuCount))
  })(),
  reporter: [['html', { open: 'never' }], ['list']],
  use: {
    baseURL: frontendURL,
    trace: process.env.CI ? 'on-first-retry' : 'off',
    screenshot: 'only-on-failure',
    video: 'off',
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },

    {
      name: 'firefox',
      use: { ...devices['Desktop Firefox'] },
    },

    {
      name: 'webkit',
      use: { ...devices['Desktop Safari'] },
    },
  ],

  /* Run your local dev server before starting the tests */
  webServer: [
    {
      command: 'bun run dev -- --hostname 127.0.0.1',
      url: frontendURL,
      reuseExistingServer: !process.env.CI,
      timeout: 120000,
      stdout: 'pipe',
      env: {
        ...process.env,
        NODE_ENV: 'development',
        PORT: frontendPort,
        NEXT_PUBLIC_API_URL: process.env.NEXT_PUBLIC_API_URL || backendURL,
        NEXT_PUBLIC_API_KEY:
          process.env.NEXT_PUBLIC_API_KEY ||
          process.env.API_KEY ||
          'dev-api-key-change-in-production',
      },
    },
    {
      command: 'cd ../backend && go run cmd/crm-api/main.go',
      url: `${backendURL}/health`,
      reuseExistingServer: !process.env.CI,
      timeout: 120000,
      stdout: 'pipe',
      env: {
        ...process.env,
        // Fallbacks for running playwright directly without make test-e2e
        DATABASE_URL:
          process.env.DATABASE_URL ||
          'postgres://crm_user:crm_password@localhost:5432/personal_crm_test?sslmode=disable',
        API_KEY: process.env.API_KEY || 'dev-api-key-change-in-production',
        PORT: backendPort,
        FRONTEND_URL: frontendURL,
        MIGRATIONS_PATH: 'migrations',
        CRM_ENV: 'testing',
        ENABLE_EXTERNAL_SYNC: 'true',
        // The Imports Interactions tab calls GET /meeting-notes/needs-attention,
        // which is only registered when event-bus ingest is enabled.
        EVENT_BUS_INGEST_ENABLED: process.env.EVENT_BUS_INGEST_ENABLED || 'true',
      },
    },
  ],
})
