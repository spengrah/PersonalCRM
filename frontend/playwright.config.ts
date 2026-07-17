import { defineConfig, devices } from '@playwright/test'
import os from 'os'
import { readFileSync } from 'fs'

// Effective CPU budget = the cgroup CFS quota when present, else the visible
// core count. A container can be capped far below os.cpus(): our dev sandbox
// reports 10 cores but cpu.max limits it to 2. Sizing the worker pool to the
// visible cores oversubscribes such a box, which measured SLOWER than matching
// the quota (full suite: 4 workers on a 2-CPU cap = 562s vs 440s at 2 workers).
// Falls back to os.cpus() on macOS / cgroup v1 / unlimited quota.
function readCgroupV2CpuMax(): string | null {
  // The applicable cpu.max lives at this process's cgroup-v2 path, which is
  // NOT always the root (nested systemd scopes put it deeper); reading the root
  // there returns "max" and misses the cap. Resolve the path from
  // /proc/self/cgroup ("0::/<path>"), then fall back to the root.
  const paths: string[] = []
  try {
    const line = readFileSync('/proc/self/cgroup', 'utf8')
      .split('\n')
      .find(l => l.startsWith('0::'))
    if (line) paths.push(`/sys/fs/cgroup${line.slice(3).trim()}/cpu.max`)
  } catch {
    // /proc/self/cgroup unreadable — fall through to the root path
  }
  paths.push('/sys/fs/cgroup/cpu.max')
  for (const p of paths) {
    try {
      const raw = readFileSync(p, 'utf8').trim()
      if (raw && raw !== 'max') return raw
    } catch {
      // try the next candidate path
    }
  }
  return null
}

function effectiveCpuBudget(): number {
  const cores = os.cpus().length || 1
  const raw = readCgroupV2CpuMax()
  if (raw) {
    const [quotaStr, periodStr] = raw.split(/\s+/)
    const quota = Number(quotaStr)
    const period = Number(periodStr)
    if (quota > 0 && period > 0) {
      // Clamp to >=1: a sub-one-CPU quota floors to 0, but the pool still needs
      // at least one worker (and must not fall back to all visible cores).
      return Math.min(cores, Math.max(1, Math.floor(quota / period)))
    }
  }
  return cores
}

const frontendPort = process.env.E2E_FRONTEND_PORT || '3000'
const backendPort = process.env.E2E_BACKEND_PORT || '8080'
const frontendURL = `http://localhost:${frontendPort}`
const backendURL = `http://localhost:${backendPort}`

// CI serves a production standalone build (built ahead of time in the e2e job,
// with `.next/static` + `public` copied alongside `server.js` the way deploy
// does) to remove next dev's on-demand route compilation from the timed run.
// Locally (CI unset) we keep `next dev` for fast iteration and no build cost.
// Value-based check (the string 'true') so a local `CI=0`/`CI=false` does not
// select the prod path. `next start` is deliberately NOT used: it is disclaimed
// under `output: 'standalone'`, so we run the standalone server directly.
const isCI = process.env.CI === 'true'
const frontendCommand = isCI
  ? 'node .next/standalone/server.js'
  : 'bun run dev -- --hostname 127.0.0.1'
const frontendNodeEnv = isCI ? 'production' : 'development'

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  // Size the worker pool to the effective CPU budget (cgroup quota, not just
  // visible cores); override with PLAYWRIGHT_WORKERS. This replaces the former
  // arm64-linux=1 special-case: that was a proxy for "CPU-constrained", but the
  // real signal is the quota, and it wrongly pinned any capable arm64 Linux box
  // to 1 while missing x86 containers that are also capped.
  workers: (() => {
    const configured = Number.parseInt(process.env.PLAYWRIGHT_WORKERS || '', 10)
    if (Number.isFinite(configured) && configured > 0) {
      return configured
    }

    const cpuCount = effectiveCpuBudget()
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
      command: frontendCommand,
      url: frontendURL,
      reuseExistingServer: !process.env.CI,
      timeout: 120000,
      stdout: 'pipe',
      env: {
        ...process.env,
        NODE_ENV: frontendNodeEnv,
        PORT: frontendPort,
        HOSTNAME: '127.0.0.1',
        NEXT_PUBLIC_API_URL: process.env.NEXT_PUBLIC_API_URL || backendURL,
        NEXT_PUBLIC_API_KEY:
          process.env.NEXT_PUBLIC_API_KEY ||
          process.env.API_KEY ||
          'dev-api-key-change-in-production',
      },
    },
    {
      command: 'cd ../backend && go run ./cmd/crm-api',
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
