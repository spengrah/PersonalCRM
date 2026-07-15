// The verifier registry: behavior id → pure verifier over that behavior's captures.

import type { Verifier } from '../types'

// Every verifier then-item has migrated to Playwright E2E, so no behavior
// registers a verifier; the remaining dispatch machinery is removed with the
// residual grader cleanup.
export const VERIFIERS: Record<string, Verifier> = {}
