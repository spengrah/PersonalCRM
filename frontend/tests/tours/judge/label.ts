// The labeling CLI (design D6 / invariant §4.1). Pre-fills DRAFT per-item
// verdicts + critiques using a DIFFERENT/STRONGER model than the judge (breaking
// the "no LLM-generated ground truth" circularity by construction), and writes a
// *.draft.json the maintainer later CORRECTS in place into *.labeled.json
// (re-running nothing).
//
// MERGE GATE = the non-model MACHINERY (scaffolding + artifact structure),
// unit-tested with a MOCKED stronger-model drafter (offline, no quota). The REAL
// model-drafted *.draft.json is a MANUAL authoring artifact committed like the
// corpus captures — never a CI gate (see judge/DEFERRED.md).

import type { Capture } from '../support/types'
import type { Judge, PerItemVerdict } from './adapter/types'
import type { IntentSpec } from './intent-catalog'
import { runIntentPass } from './intent-runner'
import { buildJudgeInput, judgeItemsFor } from './judge-input'
import { runVerifiers } from './grader/grade'
import type { VerifierItemVerdicts } from './grader/types'

export interface DraftItem {
  then_index: number
  draft_verdict: 'pass' | 'fail' | 'unsure'
  draft_critique: string
  status: 'draft' // the maintainer flips to 'human-confirmed' when correcting
}

export interface DraftArtifact {
  case_id: string
  behavior_id: string
  drafted_by: string
  note: string
  items: DraftItem[]
}

const DRAFT_NOTE =
  'DRAFT labels from a stronger model — NOT ground truth. The maintainer corrects ' +
  'these into *.labeled.json before they gate anything (see judge/DEFERRED.md).'

// Pure: assemble a draft artifact from a stronger model's verdicts. Only the
// behavior's residue (judge-tagged plus dynamically unbound) items are
// drafted; the caller supplies the verifier verdicts that determine the
// unbound set (mirroring the two-phase runner).
export function buildDraftArtifact(
  caseId: string,
  behaviorId: string,
  draftedBy: string,
  verdicts: PerItemVerdict[],
  verifierVerdicts?: VerifierItemVerdicts
): DraftArtifact {
  const wanted = new Set(judgeItemsFor(behaviorId, verifierVerdicts).map(i => i.itemIndex))
  const items: DraftItem[] = verdicts
    .filter(v => wanted.has(v.itemIndex))
    .sort((a, b) => a.itemIndex - b.itemIndex)
    .map(v => ({
      then_index: v.itemIndex,
      draft_verdict: v.verdict,
      draft_critique: v.critique,
      status: 'draft' as const,
    }))
  return {
    case_id: caseId,
    behavior_id: behaviorId,
    drafted_by: draftedBy,
    note: DRAFT_NOTE,
    items,
  }
}

// Draft one case: build the judge input for the residue items, call the DRAFTER
// (a stronger model than the runtime judge), assemble the artifact. Testable
// with a mocked drafter — no quota.
export async function draftForCase(
  caseId: string,
  behaviorId: string,
  captures: Capture[],
  drafter: Judge,
  draftedBy: string
): Promise<DraftArtifact> {
  const verifierVerdicts = runVerifiers({ behaviorId, captures })
  const input = buildJudgeInput(behaviorId, captures)
  const verdicts = input && input.items.length > 0 ? await drafter(input) : []
  return buildDraftArtifact(caseId, behaviorId, draftedBy, verdicts, verifierVerdicts)
}

// One intent-case draft: DEFERRED.md's "the Claude drafter covers intents too".
// Mirrors the eval's --judge intent loop (runIntentPass over the case's
// possibly-mutated captures), so the drafter grades exactly what the runtime
// judge would — including the grounding downgrade of an uncited fail.
export interface IntentDraftArtifact {
  intent_case_id: string
  intent_id: string
  drafted_by: string
  note: string
  draft_verdict: 'pass' | 'fail' | 'unsure'
  draft_citation: string
  draft_critique: string
  status: 'draft'
}

export async function draftForIntentCase(
  intentCaseId: string,
  spec: IntentSpec,
  captures: Capture[],
  drafter: Judge,
  draftedBy: string
): Promise<IntentDraftArtifact> {
  const [grade] = await runIntentPass(captures, drafter, [spec])
  return {
    intent_case_id: intentCaseId,
    intent_id: spec.id,
    drafted_by: draftedBy,
    note: DRAFT_NOTE,
    draft_verdict: grade.verdict,
    draft_citation: grade.citation ?? '',
    draft_critique: grade.reason ?? '',
    status: 'draft',
  }
}

// CLI (bun): draft labels for every case, writing *.draft.json.
// QA_LABELER selects the (stronger) drafter profile — DISTINCT from QA_JUDGE.
async function main(): Promise<void> {
  const fs = await import('fs')
  const path = await import('path')
  const { loadCorpus } = await import('./corpus/load')
  const { selectJudge } = await import('./adapter')
  const { applyMutation, resolveCaseCaptures } = await import('./doctor')
  const { intentSpec } = await import('./intent-catalog')

  const corpusRoot = process.argv[2] ?? path.join(import.meta.dirname ?? __dirname, 'corpus')
  const outDir = process.argv[3] ?? path.join(corpusRoot, 'labels')
  // Default drafter: claude — a DIFFERENT model family from the codex runtime
  // judge, per the circularity-breaking rule (judge/DEFERRED.md).
  const labelerProfile = process.env.QA_LABELER ?? 'claude'
  // QA_LABELER_MODEL selects the stronger drafter model AND is stamped in
  // drafted_by — it must actually reach the adapter, not just the label.
  const labelerModel = process.env.QA_LABELER_MODEL
  const drafter = selectJudge(labelerProfile, labelerModel)
  const draftedBy = `${labelerProfile}${labelerModel ? `:${labelerModel}` : ''}`

  const { cases, intentCases, capturesFor } = loadCorpus(corpusRoot)
  fs.mkdirSync(outDir, { recursive: true })
  for (const c of cases) {
    // Draft over the SAME captures the eval grades — for a doctored case that
    // means the mutated evidence, so the draft describes the doctored world.
    // The residue check runs the verifiers over those captures so dynamically
    // unbound items are drafted too (not just statically judge-tagged ones).
    const captures = resolveCaseCaptures(c, capturesFor(c))
    const vv = runVerifiers({ behaviorId: c.behavior_id, captures })
    if (judgeItemsFor(c.behavior_id, vv).length === 0) continue
    const artifact = await draftForCase(c.id, c.behavior_id, captures, drafter, draftedBy)
    const outPath = path.join(outDir, `${c.id}.draft.json`)
    fs.writeFileSync(outPath, `${JSON.stringify(artifact, null, 2)}\n`, 'utf8')
    console.log(`labeled (draft): ${outPath}`)
  }

  // Intent-case drafts (same recipe; DEFERRED.md "the Claude drafter covers
  // intents too"). Doctored intent cases draft over the MUTATED evidence.
  for (const ic of intentCases) {
    const spec = intentSpec(ic.intent_id)
    if (!spec) {
      console.log(`skipped ${ic.id}: unknown intent ${ic.intent_id} (catalog drift?)`)
      continue
    }
    let captures = capturesFor(ic)
    if (ic.mutation) captures = applyMutation(captures, ic.mutation)
    const artifact = await draftForIntentCase(ic.id, spec, captures, drafter, draftedBy)
    const outPath = path.join(outDir, `${ic.id}.draft.json`)
    fs.writeFileSync(outPath, `${JSON.stringify(artifact, null, 2)}\n`, 'utf8')
    console.log(`labeled (draft): ${outPath}`)
  }
}

if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main()
}
