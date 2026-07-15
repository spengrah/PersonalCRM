import { describe, it, expect } from 'vitest'
import { SPEC_CATALOG } from './spec-catalog'
import { classificationFor } from './grader/classification'

// The judge-residue ID set (D5 completeness guard): spec-catalog.ts is a
// hand-transcribed copy of the YAML SSOT, so a behavior dropped from the catalog
// must fail this test rather than silently vanish from the coverage report. The
// verifier catalog migrated to E2E; only the two behaviors carrying a judge item
// remain.
const RESIDUE_IDS = ['CON-042', 'DSH-004']

describe('spec catalog', () => {
  it('covers EXACTLY the judge-residue behaviors (D5 completeness guard)', () => {
    expect(Object.keys(SPEC_CATALOG).sort()).toEqual(RESIDUE_IDS)
  })

  it('preserves each survivor behavior verbatim — full given/when/then (SSOT transcription)', () => {
    // A deep-equality guard so truncating or editing a survivor's clauses (e.g.
    // dropping CON-042's "cannot be undone" then-item, which the judge grades)
    // fails here rather than silently shrinking the judge prompt.
    expect(SPEC_CATALOG['CON-042']).toEqual({
      id: 'CON-042',
      title: 'Deleting a contact requires explicit confirmation',
      given: 'a contact detail page',
      when: 'the user asks to delete the contact',
      then: [
        'a confirmation prompt warns the action cannot be undone',
        'only on confirmation is the contact deleted',
        'on success the user is returned to the contact list',
      ],
    })
    expect(SPEC_CATALOG['DSH-004']).toEqual({
      id: 'DSH-004',
      title: 'The overdue widget distinguishes loading and error states from its content',
      given: 'the overdue widget is loading or its request has failed',
      when: 'the dashboard renders',
      then: [
        'while loading, placeholder content is shown rather than an empty or caught-up state',
        'on request failure, an error state with a failure reason is shown rather than an empty or caught-up state',
        'the shown failure reason faithfully reflects the actual failure',
      ],
    })
  })

  it('every classification row indexes within its catalog then-items (subset)', () => {
    // The classification is an index-faithful SUBSET of the catalog then-items:
    // a surviving judge row may sit at a non-zero / gapped index (e.g.
    // DSH-004[2]), so this is a ceiling check, not a count-equality.
    for (const [id, spec] of Object.entries(SPEC_CATALOG)) {
      for (const c of classificationFor(id)) {
        expect(c.thenIndex, `${id}[${c.thenIndex}] within then.length`).toBeLessThan(
          spec.then.length
        )
      }
    }
  })
})
