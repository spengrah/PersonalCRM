// Judge selection by the QA_JUDGE env (default codex-exec). The codex-SDK impl
// is DEFERRED (design D2 / judge/DEFERRED.md): there is no `@openai/codex-sdk`
// import (an unresolvable import would fail tsc), so selecting it throws with a
// pointer to the follow-up.

import { makeCodexExecJudge } from './codex-exec'
import { makeHttpJudge } from './http'
import type { Judge } from './types'

export type JudgeKind = 'codex-exec' | 'http' | 'codex-sdk'

// `model` overrides the adapter's default model env, so a caller (e.g. the
// labeling CLI passing QA_LABELER_MODEL) selects a specific/stronger model
// rather than falling back to the judge's model env.
export function selectJudge(
  kind: string = process.env.QA_JUDGE ?? 'codex-exec',
  model?: string
): Judge {
  switch (kind) {
    case 'codex-exec':
      return makeCodexExecJudge(model ? { model } : {})
    case 'http':
      return makeHttpJudge(model ? { model } : {})
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
