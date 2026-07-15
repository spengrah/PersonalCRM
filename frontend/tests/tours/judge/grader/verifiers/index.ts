// The verifier registry: behavior id → pure verifier over that behavior's captures.

import type { Verifier } from '../types'
import { cad030 } from './cad030'
import { cad031 } from './cad031'
import { cad033 } from './cad033'

export const VERIFIERS: Record<string, Verifier> = {
  'CAD-030': cad030,
  'CAD-031': cad031,
  'CAD-033': cad033,
}

export { cad030, cad031, cad033 }
