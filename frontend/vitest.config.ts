import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import path from 'path'

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
