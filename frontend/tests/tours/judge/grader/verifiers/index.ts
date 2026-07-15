// The verifier registry: behavior id → pure verifier over that behavior's captures.

import type { Verifier } from '../types'
import { dsh003 } from './dsh003'
import { dsh005 } from './dsh005'
import { dsh007 } from './dsh007'
import { cad026 } from './cad026'
import { cad027 } from './cad027'
import { cad028 } from './cad028'
import { cad029 } from './cad029'
import { cad030 } from './cad030'
import { cad031 } from './cad031'
import { cad033 } from './cad033'

export const VERIFIERS: Record<string, Verifier> = {
  'DSH-003': dsh003,
  'DSH-005': dsh005,
  'DSH-007': dsh007,
  'CAD-026': cad026,
  'CAD-027': cad027,
  'CAD-028': cad028,
  'CAD-029': cad029,
  'CAD-030': cad030,
  'CAD-031': cad031,
  'CAD-033': cad033,
}

export { dsh003, dsh005, dsh007, cad026, cad027, cad028, cad029, cad030, cad031, cad033 }
