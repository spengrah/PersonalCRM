// Bridge captures → a JudgeInput for the residue items. Used by the eval's
// --judge advisory layer and the labeling CLI (both feed the judge/labeler the
// same evidence bundle). Pure — no model, no fs.

import type { Capture } from '../support/types'
import type { EvidenceBlocks, JudgeInput, JudgeItem } from './adapter/types'
import { classificationFor } from './grader/classification'
import { runVerifiers } from './grader/grade'
import type { VerifierItemVerdicts } from './grader/types'
import { captureSection, type ScreenshotResolver } from './intent-input'
import { behaviorSpec } from './spec-catalog'

// Aggregate a behavior's captures into one evidence bundle: all dialogs, a
// merged aria root (so every visible text node is present), merged api, the
// first frame's url + serverTime. Retained for callers that need the flat
// bundle (self-consistency repeats); judge prompts now prefer the per-capture
// sections below, which keep in-flight vs settled states distinguishable.
export function buildEvidence(captures: Capture[]): EvidenceBlocks {
  const aria = {
    role: 'root' as const,
    children: captures.flatMap(c => c.aria.children ?? []),
  }
  const api = Object.assign({}, ...captures.map(c => c.apiResponses))
  return {
    url: captures[0]?.url,
    aria,
    api,
    serverTime: captures[0]?.serverTime,
    dialogs: captures.flatMap(c => c.dialogs),
  }
}

// The residue items a judge grades for a behavior: judge-tagged items plus any
// verifier items that emitted `unbound` (the anchor was not found in present
// evidence — the judge reasons over the same aria without the copy pin).
export function judgeItemsFor(
  behaviorId: string,
  verifierVerdicts?: VerifierItemVerdicts
): JudgeItem[] {
  const spec = behaviorSpec(behaviorId)
  if (!spec) return []
  return classificationFor(behaviorId)
    .filter(c => c.grader === 'judge' || verifierVerdicts?.[c.thenIndex]?.verdict === 'unbound')
    .map(c => ({ itemIndex: c.thenIndex, thenText: spec.then[c.thenIndex] ?? '' }))
}

export function buildJudgeInput(
  behaviorId: string,
  captures: Capture[],
  // Default: discover the FULL residue — judge-tagged plus dynamically
  // unbound items — by running the (pure, cheap) verifiers over the given
  // captures, so direct callers (labeling CLI, self-consistency) stay in
  // lockstep with the two-phase runner.
  items: JudgeItem[] = judgeItemsFor(behaviorId, runVerifiers({ behaviorId, captures })),
  resolveScreenshot?: ScreenshotResolver
): JudgeInput | undefined {
  const spec = behaviorSpec(behaviorId)
  if (!spec) return undefined
  // Screenshots attach all-or-nothing, mirroring the intent pass: a gap would
  // silently misalign images against the CAPTURE[n] sections.
  const resolved = resolveScreenshot ? captures.map(resolveScreenshot) : []
  const images =
    resolved.length > 0 && resolved.every((p): p is string => p !== undefined) ? resolved : []
  return {
    behaviorId,
    behaviorTitle: spec.title,
    given: spec.given,
    when: spec.when,
    then: spec.then,
    items,
    evidence: buildEvidence(captures),
    captureSections: captures.map(captureSection),
    ...(images.length > 0 ? { images } : {}),
  }
}
