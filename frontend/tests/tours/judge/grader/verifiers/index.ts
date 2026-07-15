// The verifier registry: behavior id → pure verifier over that behavior's captures.

import type { Verifier } from '../types'

// All verifier then-items migrated to Playwright E2E (arc PR1/PR2/PR3); the
// residual dispatch machinery is removed in PR4.
export const VERIFIERS: Record<string, Verifier> = {}
