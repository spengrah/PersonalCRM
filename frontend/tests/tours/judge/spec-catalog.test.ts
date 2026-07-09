import { describe, it, expect } from 'vitest'
import { SPEC_CATALOG } from './spec-catalog'
import { classificationFor } from './grader/classification'

describe('spec catalog', () => {
  it('covers the 7 current contacts ux behaviors', () => {
    expect(Object.keys(SPEC_CATALOG).sort()).toEqual([
      'CON-038',
      'CON-040',
      'CON-041',
      'CON-042',
      'CON-043',
      'CON-044',
      'CON-045',
    ])
  })

  it('each behavior has as many then-items as the classification map', () => {
    for (const [id, spec] of Object.entries(SPEC_CATALOG)) {
      expect(spec.then.length, `${id} then-count`).toBe(classificationFor(id).length)
    }
  })
})
