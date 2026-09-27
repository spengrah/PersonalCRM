import { describe, it, expect } from 'vitest'
import { SPEC_CATALOG } from './spec-catalog'
import { classificationFor } from './grader/classification'
import { loadSpecBehaviors } from './spec-yaml'

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

  it('transcribes each survivor verbatim from spec YAML — full given/when/then', () => {
    // Deep equality against the YAML SSOT, so editing or truncating a survivor's
    // clauses on either side (e.g. dropping CON-042's "cannot be undone"
    // then-item, which the judge grades) fails here rather than silently
    // changing the judge prompt.
    const yaml = new Map(loadSpecBehaviors().map(b => [b.id, b]))
    for (const id of RESIDUE_IDS) {
      const b = yaml.get(id)
      expect(b, `${id} missing from spec YAML`).toBeDefined()
      expect(SPEC_CATALOG[id]).toEqual({
        id: b!.id,
        title: b!.title,
        given: b!.given,
        when: b!.when,
        then: (b!.then ?? []).map(t => t.text),
      })
    }
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
