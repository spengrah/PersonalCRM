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
