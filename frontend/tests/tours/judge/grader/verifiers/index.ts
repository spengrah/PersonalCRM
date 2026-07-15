// The verifier registry: behavior id → pure verifier over that behavior's captures.

import type { Verifier } from '../types'
import { cad033 } from './cad033'

export const VERIFIERS: Record<string, Verifier> = {
  'CAD-033': cad033,
}

export { cad033 }
