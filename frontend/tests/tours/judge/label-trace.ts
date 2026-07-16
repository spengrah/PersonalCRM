// The label-trace contract (spec lines 51–53): the structured evidence a
// Langfuse reviewer needs to adjudicate a verdict WITHOUT leaving Langfuse.
//
// This module is PURE (no fs, no model, no Langfuse). It projects a `JudgeInput`
// into two carriers that ride the span (`adapter/span.ts` → `qa.scenario` /
// `qa.graded_evidence`) and are rendered per-item into trace bodies by the
// exporter (`export/langfuse.ts`):
//
//   - `Scenario` — the full scenario a reviewer grades against: the behavior's
//     GWT + every graded residue item + the full then-list, OR the intent's
//     goal/statement + status. The exporter fans one span out to ONE trace per
//     graded item, each carrying that item's SINGULAR `then_text` (spec's field
//     contract), while the span stays per-judge-call.
//   - `GradedEvidenceEntry[]` — the graded captures in the prompt's own
//     CAPTURE[n] order, each carrying its REAL `captureFile` (spec line 53) and
//     its OWN screenshot (index-attributed; the flat media gallery is orderless
//     and thus unattributable). All-or-nothing (INV-4): when the run degraded to
//     aria-only, every entry's `screenshot` is undefined — honest, not a bug.
//
// The `screenshot_caveat` is NOT built here as a field — it is DERIVED by the
// exporter from the presence of `mutation` (a doctored trace has both, a real
// trace has neither), so `SCREENSHOT_CAVEAT` lives here only as the shared text.

import type { EvidenceBlocks, JudgeInput } from './adapter/types'

// The note a reviewer sees on doctored evidence. Fixed synthetic-free text —
// emitted iff the trace carries a `mutation` (the two are inseparable; see D6).
export const SCREENSHOT_CAVEAT =
  'Screenshots show the UNDOCTORED world. The doctoring is JSON-only (aria/api/fields), so the pixels and the graded evidence intentionally disagree — grade the evidence blocks, not the screenshot.'

// One graded capture, attributed to its real source file, with its own optional
// screenshot. On the SPAN, `screenshot` is an ABSOLUTE PATH; the exporter
// resolves it to a media token per item-trace and never ships the raw path.
export interface GradedEvidenceEntry {
  captureFile: string
  note: string
  evidence: EvidenceBlocks
  screenshot?: string
}

// The scenario a reviewer grades against. Discriminated so ONE carrier serves
// both a behavior judge call (GWT + a LIST of graded residue items) and an
// intent judge call (goal/statement + status, no GWT).
export type Scenario =
  | {
      kind: 'behavior'
      behaviorId: string
      behaviorTitle: string
      given: string
      when: string
      // EVERY graded item in this judge call — the trace is per judge CALL, and a
      // behavior call can grade >1 residue item; the exporter fans out to one
      // trace per item so each honors the contract's singular `then_text`.
      items: { itemIndex: number; thenText: string }[]
      allThen: string[] // the full then-list, for context
    }
  | {
      kind: 'intent'
      intentId: string
      title: string
      statement: string
      status: 'current' | 'proposed'
    }

// Project a JudgeInput into its Scenario. Intent inputs → the intent variant;
// everything else → the behavior variant carrying all graded items + the full
// then-list.
export function buildScenario(input: JudgeInput): Scenario {
  if (input.intent) {
    return {
      kind: 'intent',
      intentId: input.behaviorId,
      title: input.behaviorTitle,
      statement: input.intent.statement,
      status: input.intent.status,
    }
  }
  return {
    kind: 'behavior',
    behaviorId: input.behaviorId,
    behaviorTitle: input.behaviorTitle,
    given: input.given,
    when: input.when,
    items: input.items.map(i => ({ itemIndex: i.itemIndex, thenText: i.thenText })),
    allThen: input.then,
  }
}

// The graded captures, index-aligned to the prompt's CAPTURE[n] sections and to
// `images` (the SAME array the prompt renders — INV-4). Entry n's `screenshot`
// is `images[n]` (an absolute path) or undefined when `images` is empty (the
// aria-only degrade: the input builders drop ALL images on any gap, so this
// stays all-or-nothing). Reads the REAL filename from `section.captureFile`.
export function buildGradedEvidence(input: JudgeInput, images: string[]): GradedEvidenceEntry[] {
  const sections = input.captureSections ?? []
  return sections.map((s, n) => ({
    captureFile: s.captureFile,
    note: s.note,
    evidence: s.evidence,
    screenshot: images[n],
  }))
}
