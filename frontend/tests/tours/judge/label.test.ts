import { describe, it, expect } from 'vitest'
import { buildDraftArtifact, draftForCase } from './label'
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
})
