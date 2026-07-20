// Judge selection by the QA_JUDGE env (default codex-exec). All three adapters
// sit behind the identical `Judge` interface — codex-exec (default, spawns the
// `codex` CLI), codex-sdk (the `@openai/codex-sdk` transport swap), and the http
// stub — so the concrete brain is a config swap with ZERO grader change (D2).

import { makeCodexExecJudge } from './codex-exec'
import { makeCodexSdkJudge } from './codex-sdk'
import { makeHttpJudge } from './http'
import type { Judge } from './types'

export type JudgeKind = 'codex-exec' | 'http' | 'codex-sdk'

// `model` overrides the adapter's default model env, so a caller (e.g. the
// intent pass passing QA_INTENT_MODEL) selects a specific/stronger model
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
      return makeCodexSdkJudge(model ? { model } : {})
    default:
      throw new Error(`unknown QA_JUDGE='${kind}' (expected codex-exec | codex-sdk | http)`)
  }
}

export type { Judge, JudgeInput, PerItemVerdict } from './types'
