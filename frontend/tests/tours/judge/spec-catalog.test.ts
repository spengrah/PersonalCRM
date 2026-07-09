import { describe, it, expect } from 'vitest'
import { SPEC_CATALOG } from './spec-catalog'
import { CLASSIFICATION_ITEM_COUNT, classificationFor } from './grader/classification'

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

  it('each behavior has as many then-items as the classification map', () => {
    for (const [id, spec] of Object.entries(SPEC_CATALOG)) {
      expect(spec.then.length, `${id} then-count`).toBe(classificationFor(id).length)
    }
  })

  it('the catalog then-item total equals CLASSIFICATION_ITEM_COUNT', () => {
    const total = Object.values(SPEC_CATALOG).reduce((n, s) => n + s.then.length, 0)
    expect(total).toBe(CLASSIFICATION_ITEM_COUNT)
  })
})
