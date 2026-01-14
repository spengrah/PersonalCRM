import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  // Use 3 workers for parallelism (both CI and local). Higher parallelism causes race conditions.
  workers: 3,
  reporter: 'html',
  use: {
    baseURL: 'http://localhost:3000',
    trace: 'on-first-retry',
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
      url: 'http://localhost:3000',
      reuseExistingServer: !process.env.CI,
      timeout: 120000,
      stdout: 'pipe',
      env: {
        ...process.env,
        NODE_ENV: 'development',
        PORT: '3000',
      },
    },
    {
      command: 'cd ../backend && go run cmd/crm-api/main.go',
      url: 'http://localhost:8080/health',
      reuseExistingServer: !process.env.CI,
      timeout: 120000,
      stdout: 'pipe',
      env: {
        ...process.env,
        // Fallbacks for running playwright directly without make test-e2e
        DATABASE_URL:
          process.env.DATABASE_URL ||
          'postgres://crm_user:crm_password@localhost:5432/personal_crm?sslmode=disable',
        API_KEY: process.env.API_KEY || 'dev-api-key-change-in-production',
        MIGRATIONS_PATH: 'migrations',
        CRM_ENV: 'testing',
        ENABLE_EXTERNAL_SYNC: 'true',
      },
    },
  ],
})
