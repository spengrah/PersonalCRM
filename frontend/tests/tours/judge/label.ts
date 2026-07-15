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
import { makeCodexExecJudge } from './adapter/codex-exec'
import type { Judge, PerItemVerdict } from './adapter/types'
import type { IntentSpec } from './intent-catalog'
import { DEFAULT_INTENT_EFFORT, DEFAULT_INTENT_MODEL, runIntentPass } from './intent-runner'
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
// behavior's judge-tagged residue items are drafted.
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

// One intent-case draft: DEFERRED.md's "the drafter covers intents too".
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

// The drafter must be STRONGER than the cheap runtime judge (the DEFERRED.md
// recipe), so on codex-exec the model/effort default to the intent pass's
// validated stronger tier (gpt-5.5/medium), NOT the adapter's cheap judge
// defaults (gpt-5.4-mini/low). Non-codex profiles keep their own model config
// unless QA_LABELER_MODEL is explicit — the codex default must not leak onto
// other endpoints (same rule as makeIntentJudge). Cross-family drafting
// (a Claude drafter) was considered and consciously waived by the maintainer:
// the human correction pass is the ground-truth step, so same-family drafts
// only pre-fill what the maintainer verifies — and codex rides the
// subscription quota. Pure; exported for tests.
export interface LabelerConfig {
  profile: string
  model?: string
  effort?: string
}

export function resolveLabelerDrafter(env: Record<string, string | undefined>): LabelerConfig {
  const profile = env.QA_LABELER ?? 'codex-exec'
  const isCodex = profile === 'codex-exec'
  return {
    profile,
    model: env.QA_LABELER_MODEL ?? (isCodex ? DEFAULT_INTENT_MODEL : undefined),
    effort: env.QA_LABELER_EFFORT ?? (isCodex ? DEFAULT_INTENT_EFFORT : undefined),
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
  // The resolved model/effort must actually reach the adapter AND be stamped
  // in drafted_by — never just the label.
  const cfg = resolveLabelerDrafter(process.env)
  const drafter =
    cfg.profile === 'codex-exec'
      ? makeCodexExecJudge({ model: cfg.model, effort: cfg.effort })
      : selectJudge(cfg.profile, cfg.model)
  const draftedBy = `${cfg.profile}${cfg.model ? `:${cfg.model}` : ''}`

  const { cases, intentCases, capturesFor } = loadCorpus(corpusRoot)
  fs.mkdirSync(outDir, { recursive: true })
  for (const c of cases) {
    // Draft over the case's resolved captures — for a doctored case that means
    // the mutated evidence, so the draft describes the doctored world. Skip a
    // behavior with no judge-tagged residue items (nothing to draft).
    const captures = resolveCaseCaptures(c, capturesFor(c))
    if (judgeItemsFor(c.behavior_id).length === 0) continue
    const artifact = await draftForCase(c.id, c.behavior_id, captures, drafter, draftedBy)
    const outPath = path.join(outDir, `${c.id}.draft.json`)
    fs.writeFileSync(outPath, `${JSON.stringify(artifact, null, 2)}\n`, 'utf8')
    console.log(`labeled (draft): ${outPath}`)
  }

  // Intent-case drafts (same recipe; DEFERRED.md "the drafter covers intents
  // too"). Doctored intent cases draft over the MUTATED evidence.
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
