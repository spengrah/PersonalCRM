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
import { buildJudgeInput, judgeItemsFor } from './judge-input'

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
// behavior's residue (judge + judgeFallback) items are drafted.
export function buildDraftArtifact(
  caseId: string,
  behaviorId: string,
  draftedBy: string,
  verdicts: PerItemVerdict[]
): DraftArtifact {
  const wanted = new Set(judgeItemsFor(behaviorId).map(i => i.itemIndex))
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
  const input = buildJudgeInput(behaviorId, captures)
  const verdicts = input && input.items.length > 0 ? await drafter(input) : []
  return buildDraftArtifact(caseId, behaviorId, draftedBy, verdicts)
}

// CLI (bun): draft labels for every case, writing *.draft.json.
// QA_LABELER selects the (stronger) drafter profile — DISTINCT from QA_JUDGE.
async function main(): Promise<void> {
  const fs = await import('fs')
  const path = await import('path')
  const { loadCorpus } = await import('./corpus/load')
  const { selectJudge } = await import('./adapter')
  const { resolveCaseCaptures } = await import('./doctor')

  const corpusRoot = process.argv[2] ?? path.join(import.meta.dirname ?? __dirname, 'corpus')
  const outDir = process.argv[3] ?? path.join(corpusRoot, 'labels')
  const labelerProfile = process.env.QA_LABELER ?? 'codex-exec'
  // QA_LABELER_MODEL selects the stronger drafter model AND is stamped in
  // drafted_by — it must actually reach the adapter, not just the label.
  const labelerModel = process.env.QA_LABELER_MODEL
  const drafter = selectJudge(labelerProfile, labelerModel)
  const draftedBy = `${labelerProfile}${labelerModel ? `:${labelerModel}` : ''}`

  const { cases, capturesFor } = loadCorpus(corpusRoot)
  fs.mkdirSync(outDir, { recursive: true })
  for (const c of cases) {
    if (judgeItemsFor(c.behavior_id).length === 0) continue
    // Draft over the SAME captures the eval grades — for a doctored case that
    // means the mutated evidence, so the draft describes the doctored world.
    const captures = resolveCaseCaptures(c, capturesFor(c))
    const artifact = await draftForCase(c.id, c.behavior_id, captures, drafter, draftedBy)
    const outPath = path.join(outDir, `${c.id}.draft.json`)
    fs.writeFileSync(outPath, `${JSON.stringify(artifact, null, 2)}\n`, 'utf8')
    console.log(`labeled (draft): ${outPath}`)
  }
}

if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main()
}
