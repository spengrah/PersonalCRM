// Judge selection by the QA_JUDGE env. All three adapters sit behind the
// identical `Judge` interface — codex-sdk (the default `@openai/codex-sdk`
// transport), codex-exec (spawns the `codex` CLI), and the http stub — so the
// concrete brain is a config swap with ZERO grader change (D2).

import { makeCodexExecJudge } from './codex-exec'
import { makeCodexSdkJudge } from './codex-sdk'
import { makeHttpJudge } from './http'
import type { Judge } from './types'

export type JudgeKind = 'codex-exec' | 'http' | 'codex-sdk'

// The transport an unconfigured run gets, in ONE place — every QA_JUDGE fallback
// in the harness reads this rather than repeating a literal, so the default can
// never drift between the residue pass, the intent pass and the report CLI's
// image-capability gate.
//
// codex-sdk, not codex-exec: the exec event stream carries no cached-input count
// (its `input_tokens` is inclusive of cache reads), so an unconfigured round on
// that transport prices every cached token at the full input rate and reports the
// overstatement as authoritative. The default has to be the transport that
// reports usage correctly.
export const DEFAULT_JUDGE_KIND: JudgeKind = 'codex-sdk'

// `model` overrides the adapter's default model env, so a caller (e.g. the
// intent pass passing QA_INTENT_MODEL) selects a specific/stronger model
// rather than falling back to the judge's model env.
export function selectJudge(
  kind: string = process.env.QA_JUDGE ?? DEFAULT_JUDGE_KIND,
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
