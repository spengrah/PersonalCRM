// The judge adapter contract: a narrow, swappable `judge(input) → verdict[]`
// seam. Everything downstream (the grader's residue path) is insulated behind
// this interface, so the concrete brain (codex-exec now; codex-sdk / HTTP later)
// is a config swap with ZERO grader change (design D2).

import type { AriaNode, ApiResponses, DialogRecord, ServerTimeFrame } from '../../support/types'

// One residual then-item the judge must grade.
export interface JudgeItem {
  itemIndex: number
  thenText: string
}

// The evidence blocks presented to the judge (fixed labeled-block order; D4).
export interface EvidenceBlocks {
  url?: string
  aria?: AriaNode
  api?: ApiResponses
  serverTime?: ServerTimeFrame
  dialogs?: DialogRecord[]
}

export interface JudgeInput {
  behaviorId: string
  behaviorTitle: string
  given: string
  when: string
  then: string[] // full then list (context)
  items: JudgeItem[] // the residual items to grade
  evidence: EvidenceBlocks
}

// Categorical per-item verdict. `citation` is the exact aria node label / JSON
// path the model bound to; the grounding rule (grade.ts) downgrades an uncited
// `fail` to `unsure`.
export interface PerItemVerdict {
  itemIndex: number
  verdict: 'pass' | 'fail' | 'unsure'
  citation: string
  critique: string
}

export type Judge = (input: JudgeInput) => Promise<PerItemVerdict[]>
