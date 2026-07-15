import { describe, it, expect } from 'vitest'
import { SPEC_CATALOG } from './spec-catalog'
import { classificationFor } from './grader/classification'

// The current-ux first-cut ID set (D5 completeness guard): spec-catalog.ts is a
// hand-transcribed copy of the YAML SSOT, so a behavior dropped from the catalog
// must fail this test rather than silently vanish from the coverage report.
const FIRST_CUT_CURRENT_UX_IDS = [
  'CAD-026',
  'CAD-027',
  'CAD-028',
  'CAD-029',
  'CAD-030',
  'CAD-031',
  'CAD-033',
  'CON-038',
  'CON-040',
  'CON-041',
  'CON-042',
  'CON-043',
  'CON-044',
  'CON-045',
  'DSH-001',
  'DSH-002',
  'DSH-003',
  'DSH-004',
  'DSH-005',
  'DSH-007',
]

describe('spec catalog', () => {
  it('covers EXACTLY the 20 current first-cut ux behaviors (D5 completeness guard)', () => {
    expect(Object.keys(SPEC_CATALOG).sort()).toEqual(FIRST_CUT_CURRENT_UX_IDS)
  })

  it('every classification row indexes within its catalog then-items (subset)', () => {
    // Post-migration the classification is an index-faithful SUBSET of the
    // catalog then-items (verifier rows leave as they migrate to E2E; the
    // catalog is not pruned until PR4), so this is a ceiling check, not a
    // count-equality.
    for (const [id, spec] of Object.entries(SPEC_CATALOG)) {
      for (const c of classificationFor(id)) {
        expect(c.thenIndex, `${id}[${c.thenIndex}] within then.length`).toBeLessThan(
          spec.then.length
        )
      }
    }
  })
})
