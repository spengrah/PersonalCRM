import { describe, it, expect } from 'vitest'
import { buildDraftArtifact, draftForCase } from './label'
import { resolveCaseCaptures } from './doctor'
import { cap } from './grader/fixtures'
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

  it('does not call the drafter for a behavior with no residue items (CON-044)', async () => {
    let called = false
    const drafter: Judge = async () => {
      called = true
      return []
    }
    const artifact = await draftForCase(
      'CON-044-clean',
      'CON-044',
      [cap({ behaviors: ['CON-044'] })],
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

  it('drafts dynamically-unbound residue (no statically judge-tagged items needed)', async () => {
    // CON-041 has no judge-tagged items; a present capture whose surface
    // heading is renamed makes [0] emit `unbound`, which the labeling flow
    // must draft (the old static-fallback flow would have skipped it).
    const drafter: Judge = async input =>
      input.items.map(i => ({
        itemIndex: i.itemIndex,
        verdict: 'unsure' as const,
        citation: '',
        critique: 'renamed?',
      }))
    const captures = [
      cap({
        behaviors: ['CON-041'],
        note: 'action=edit consumed',
        url: '/contacts/x',
        aria: { role: 'root', children: [{ role: 'heading', name: 'Update Contact' }] },
      }),
    ]
    const artifact = await draftForCase('CON-041-renamed', 'CON-041', captures, drafter, 'strong')
    expect(artifact.items.map(i => i.then_index)).toContain(0)
  })
})
