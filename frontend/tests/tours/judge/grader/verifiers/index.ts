// The verifier registry: behavior id → pure verifier over that behavior's captures.

import type { Verifier } from '../types'
import { con038 } from './con038'
import { con040 } from './con040'
import { con041 } from './con041'
import { con042 } from './con042'
import { con043 } from './con043'
import { con044 } from './con044'
import { con045 } from './con045'

export const VERIFIERS: Record<string, Verifier> = {
  'CON-038': con038,
  'CON-040': con040,
  'CON-041': con041,
  'CON-042': con042,
  'CON-043': con043,
  'CON-044': con044,
  'CON-045': con045,
}

export { con038, con040, con041, con042, con043, con044, con045 }
