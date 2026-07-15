// The verifier registry: behavior id → pure verifier over that behavior's captures.

import type { Verifier } from '../types'
import { cad028 } from './cad028'
import { cad029 } from './cad029'
import { cad030 } from './cad030'
import { cad031 } from './cad031'
import { cad033 } from './cad033'

export const VERIFIERS: Record<string, Verifier> = {
  'CAD-028': cad028,
  'CAD-029': cad029,
  'CAD-030': cad030,
  'CAD-031': cad031,
  'CAD-033': cad033,
}

export { cad028, cad029, cad030, cad031, cad033 }
