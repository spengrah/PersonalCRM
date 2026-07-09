// Judge selection by the QA_JUDGE env (default codex-exec). The codex-SDK impl
// is DEFERRED (design D2 / judge/DEFERRED.md): PR2 ships no `@openai/codex-sdk`
// import (an unresolvable import would fail tsc), so selecting it throws with a
// pointer to the follow-up.

import { makeCodexExecJudge } from './codex-exec'
import { makeHttpJudge } from './http'
import type { Judge } from './types'

export type JudgeKind = 'codex-exec' | 'http' | 'codex-sdk'

export function selectJudge(kind: string = process.env.QA_JUDGE ?? 'codex-exec'): Judge {
  switch (kind) {
    case 'codex-exec':
      return makeCodexExecJudge()
    case 'http':
      return makeHttpJudge()
    case 'codex-sdk':
      throw new Error(
        'QA_JUDGE=codex-sdk is a deferred follow-up (add @openai/codex-sdk + the impl behind the ' +
          'identical Judge interface) — see judge/DEFERRED.md. Use codex-exec (default) or http.'
      )
    default:
      throw new Error(`unknown QA_JUDGE='${kind}' (expected codex-exec | http)`)
  }
}

export type { Judge, JudgeInput, PerItemVerdict } from './types'
