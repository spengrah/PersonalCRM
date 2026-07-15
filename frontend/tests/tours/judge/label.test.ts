import { describe, it, expect } from 'vitest'
import {
  buildDraftArtifact,
  draftForCase,
  draftForIntentCase,
  resolveLabelerDrafter,
} from './label'
import { resolveCaseCaptures } from './doctor'
import { cap } from './grader/fixtures'
import { DEFAULT_INTENT_EFFORT, DEFAULT_INTENT_MODEL } from './intent-runner'
import type { IntentSpec } from './intent-catalog'
import type { Judge, PerItemVerdict } from './adapter/types'

describe('buildDraftArtifact (machinery, no model)', () => {
  it('drafts only the behavior residue items and stamps status=draft', () => {
    const verdicts: PerItemVerdict[] = [
      {
        itemIndex: 0,
        verdict: 'pass',
        citation: 'dialog message',
        critique: 'warns of irreversibility',
      },
      { itemIndex: 1, verdict: 'fail', citation: 'x', critique: 'not a residue item' },
    ]
    const artifact = buildDraftArtifact('CON-042-clean', 'CON-042', 'codex-strong', verdicts)
    // CON-042 residue = item 0 only (the judge-tagged item).
    expect(artifact.items).toEqual([
      {
        then_index: 0,
        draft_verdict: 'pass',
        draft_critique: 'warns of irreversibility',
        status: 'draft',
      },
    ])
    expect(artifact.drafted_by).toBe('codex-strong')
    expect(artifact.note).toMatch(/NOT ground truth/)
  })
})

describe('draftForCase (mocked stronger-model drafter — offline)', () => {
  it('calls the drafter with the residue items and assembles the draft', async () => {
    let receivedItems: number[] = []
    const drafter: Judge = async input => {
      receivedItems = input.items.map(i => i.itemIndex)
      return input.items.map(i => ({
        itemIndex: i.itemIndex,
        verdict: 'fail',
        citation: 'dialog',
        critique: 'drafted',
      }))
    }
    const captures = [
      cap({ behaviors: ['CON-042'], dialogs: [{ type: 'confirm', message: 'cannot be undone' }] }),
    ]
    const artifact = await draftForCase(
      'CON-042-clean',
      'CON-042',
      captures,
      drafter,
      'strong-model'
    )
    expect(receivedItems).toEqual([0])
    expect(artifact.items).toEqual([
      { then_index: 0, draft_verdict: 'fail', draft_critique: 'drafted', status: 'draft' },
    ])
  })

  it('does not call the drafter for a behavior with no residue items (CAD-028)', async () => {
    let called = false
    const drafter: Judge = async () => {
      called = true
      return []
    }
    const artifact = await draftForCase(
      'CAD-028-clean',
      'CAD-028',
      [cap({ behaviors: ['CAD-028'] })],
      drafter,
      'm'
    )
    expect(called).toBe(false)
    expect(artifact.items).toEqual([])
  })

  it('drafts a doctored case over the MUTATED evidence (dialog blanked), not the clean one', async () => {
    let sawDialogMessage: string | undefined
    const drafter: Judge = async input => {
      sawDialogMessage = input.evidence.dialogs?.[0]?.message
      return input.items.map(i => ({
        itemIndex: i.itemIndex,
        verdict: 'fail',
        citation: 'dialogs',
        critique: 'no warning',
      }))
    }
    const base = [
      cap({ behaviors: ['CON-042'], dialogs: [{ type: 'confirm', message: 'cannot be undone' }] }),
    ]
    // The labeling CLI resolves captures the SAME way (applies the mutation).
    const captures = resolveCaseCaptures(
      { source: 'doctored', doctor: { mutation: { op: 'blank_dialog' } } },
      base
    )
    await draftForCase('CON-042-doctored-nowarn', 'CON-042', captures, drafter, 'strong')
    expect(sawDialogMessage).toBe('') // the drafter saw the DOCTORED (blanked) dialog
  })

  it('drafts dynamically-unbound residue alongside the static judge item', async () => {
    // DSH-004[1] (the overdue error surface) emits `unbound` when the failure
    // bracket lacks the error heading; the labeling flow must draft it
    // ALONGSIDE DSH-004's static judge item [2] (the old static-fallback flow
    // would have skipped the dynamically unbound one).
    const drafter: Judge = async input =>
      input.items.map(i => ({
        itemIndex: i.itemIndex,
        verdict: 'unsure' as const,
        citation: '',
        critique: 'renamed?',
      }))
    const captures = [
      cap({
        behaviors: ['DSH-004'],
        pair: { id: 'd', role: 'error' },
        note: 'overdue request failed',
        aria: { role: 'root', children: [{ role: 'text', text: 'Something went wrong' }] },
      }),
    ]
    const artifact = await draftForCase('DSH-004-renamed', 'DSH-004', captures, drafter, 'strong')
    const idxs = artifact.items.map(i => i.then_index)
    expect(idxs).toContain(1)
    expect(idxs).toContain(2)
  })
})

describe('resolveLabelerDrafter (stronger-than-judge defaults)', () => {
  it('defaults to codex-exec with the stronger intent-pass tier, not the cheap judge tier', () => {
    expect(resolveLabelerDrafter({})).toEqual({
      profile: 'codex-exec',
      model: DEFAULT_INTENT_MODEL,
      effort: DEFAULT_INTENT_EFFORT,
    })
  })

  it('env overrides win over the codex defaults', () => {
    expect(
      resolveLabelerDrafter({
        QA_LABELER_MODEL: 'gpt-next',
        QA_LABELER_EFFORT: 'high',
      })
    ).toEqual({ profile: 'codex-exec', model: 'gpt-next', effort: 'high' })
  })

  it('does not leak the codex default model onto non-codex profiles', () => {
    expect(resolveLabelerDrafter({ QA_LABELER: 'http' })).toEqual({
      profile: 'http',
      model: undefined,
      effort: undefined,
    })
    expect(resolveLabelerDrafter({ QA_LABELER: 'http', QA_LABELER_MODEL: 'm' }).model).toBe('m')
  })
})

describe('draftForIntentCase (mocked stronger-model drafter — offline)', () => {
  const spec: IntentSpec = {
    id: 'DSH-010',
    title: 'at a glance',
    statement: 's',
    status: 'current',
    servedBy: ['CAD-026'],
  }

  it('drafts the intent verdict via runIntentPass and stamps status=draft', async () => {
    const drafter: Judge = async () => [
      { itemIndex: 0, verdict: 'fail', citation: 'CAPTURE[0]: heading "x"', critique: 'bad' },
    ]
    const captures = [cap({ behaviors: ['CAD-026'], note: 'overdue list' })]
    const artifact = await draftForIntentCase('DSH-010-clean', spec, captures, drafter, 'strong')
    expect(artifact).toMatchObject({
      intent_case_id: 'DSH-010-clean',
      intent_id: 'DSH-010',
      drafted_by: 'strong',
      draft_verdict: 'fail',
      draft_citation: 'CAPTURE[0]: heading "x"',
      draft_critique: 'bad',
      status: 'draft',
    })
  })

  it('inherits the grounding downgrade: an uncited fail drafts as unsure', async () => {
    const drafter: Judge = async () => [
      { itemIndex: 0, verdict: 'fail', citation: '', critique: 'vibes' },
    ]
    const captures = [cap({ behaviors: ['CAD-026'], note: 'overdue list' })]
    const artifact = await draftForIntentCase('DSH-010-clean', spec, captures, drafter, 'strong')
    expect(artifact.draft_verdict).toBe('unsure')
  })
})
