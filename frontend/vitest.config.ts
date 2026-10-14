import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import path from 'path'

// Date assertions render instants in the process zone, so pin it: a test must
// not pass or fail by the machine it runs on. A zone west of UTC, rather than
// UTC itself, renders a date-only string parsed as UTC midnight as the previous
// day, so that bug still fails a test.
process.env.TZ = 'America/New_York'

export default defineConfig({
  plugins: [react()],
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: './vitest.setup.ts',
    exclude: ['tests/e2e/**', 'node_modules/**'],
    // The default 5s ceiling flakes userEvent-heavy suites when the pre-push
    // hook runs vitest concurrently with the Go integration + E2E lanes on one
    // box (#607) — a ceiling raise never slows a passing test.
    testTimeout: 15_000,
  },
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
})
