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

// One capture rendered as its own labeled CAPTURE[n] section in an intent
// prompt (multi-capture evidence stays sectioned, never merged).
export interface CaptureSection {
  note: string
  evidence: EvidenceBlocks
}

export interface JudgeInput {
  behaviorId: string
  behaviorTitle: string
  given: string
  when: string
  then: string[] // full then list (context)
  items: JudgeItem[] // the residual items to grade
  evidence: EvidenceBlocks
  /**
   * Intent variant: set on an intent-pass call. The prompt renders an INTENT
   * block (statement instead of GWT) and per-capture CAPTURE[n] sections from
   * captureSections; `evidence` is ignored. behaviorId/behaviorTitle carry the
   * intent's id/title; items is the single statement item (index 0).
   */
  intent?: { statement: string; status: 'current' | 'proposed' }
  captureSections?: CaptureSection[]
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
