// Bridge captures → a JudgeInput for the residue items. Used by the eval's
// --judge advisory layer and the labeling CLI (both feed the judge/labeler the
// same evidence bundle). Pure — no model, no fs.

import type { Capture } from '../support/types'
import type { EvidenceBlocks, JudgeInput, JudgeItem } from './adapter/types'
import { classificationFor } from './grader/classification'
import { behaviorSpec } from './spec-catalog'

// Aggregate a behavior's captures into one evidence bundle: all dialogs, a
// merged aria root (so every visible text node is present), merged api, the
// first frame's url + serverTime.
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

// The residue items a judge grades for a behavior: judge-tagged items plus
// verifier items flagged judgeFallback.
export function judgeItemsFor(behaviorId: string): JudgeItem[] {
  const spec = behaviorSpec(behaviorId)
  if (!spec) return []
  return classificationFor(behaviorId)
    .filter(c => c.grader === 'judge' || c.judgeFallback)
    .map(c => ({ itemIndex: c.thenIndex, thenText: spec.then[c.thenIndex] ?? '' }))
}

export function buildJudgeInput(
  behaviorId: string,
  captures: Capture[],
  items: JudgeItem[] = judgeItemsFor(behaviorId)
): JudgeInput | undefined {
  const spec = behaviorSpec(behaviorId)
  if (!spec) return undefined
  return {
    behaviorId,
    behaviorTitle: spec.title,
    given: spec.given,
    when: spec.when,
    then: spec.then,
    items,
    evidence: buildEvidence(captures),
  }
}
