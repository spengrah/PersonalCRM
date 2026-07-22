// The judge adapter contract: a narrow, swappable `judge(input) → verdict[]`
// seam. Everything downstream (the grader's residue path) is insulated behind
// this interface, so the concrete brain (codex-sdk default; codex-exec; HTTP stub)
// is a config swap with ZERO grader change (design D2).

import type { AriaNode, ApiResponses, DialogRecord, ServerTimeFrame } from '../../support/types'
import type { Mutation } from '../mutation'

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
  // The REAL source capture filename (spec line 53), projected here from the
  // loader's `LoadedCapture.__sourceFile` by `captureSection`. This is the
  // load-bearing hop: captures become `CaptureSection[]` before the adapter
  // sees them, so a bare `Capture.__sourceFile` would be dropped — the identity
  // MUST ride on the section. Required so every construction carries one; a
  // section built off a capture with no `__sourceFile` gets a VISIBLE
  // `unknown:<tour>/<seq>` fallback, never a value passed off as a filename.
  captureFile: string
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
  /**
   * Absolute paths of capture-point screenshots attached as model images
   * (adapters that cannot attach images ignore these; the prompt's visual
   * framing switches on their presence).
   */
  images?: string[]
  /**
   * HARNESS-INTERNAL, NEVER emitted in a prompt: the doctoring applied to this
   * input's evidence on the trap self-test path. The adapters forward it onto
   * the span (`qa.mutation`) so the exporter can render the `mutation` +
   * DERIVE the `screenshot_caveat` (a doctored trace shows the undoctored
   * pixels). PR3's trap self-test is the SOLE producer; normal
   * `buildJudgeInput`/`buildIntentJudgeInput` never set it. Typed as the
   * relocated `Mutation` (`judge/mutation.ts`) — corpus-independent, so it
   * survives the corpus deletion the trap self-test replaces.
   */
  __trap?: { mutation: Mutation }
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
