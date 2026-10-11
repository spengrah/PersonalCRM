// Expectation budgets for Playwright specs and tours. Both configs set
// expect.timeout to DEFAULT_EXPECT_TIMEOUT_MS, so an assertion that needs no more
// than that passes no timeout option. Every other budget is one of these names,
// never a literal (enforced by ESLint in eslint.config.mjs).

export const DEFAULT_EXPECT_TIMEOUT_MS = 10_000

// Short waits: probes and negative checks that should settle quickly.
export const BRIEF_TIMEOUT_MS = 2_000
export const SHORT_TIMEOUT_MS = 5_000

// UI that waits on a backend write, a refetch, or a modal transition.
export const SLOW_TIMEOUT_MS = 15_000

// UI that waits on a background job (sync, rematch, aggregation).
export const JOB_TIMEOUT_MS = 45_000
