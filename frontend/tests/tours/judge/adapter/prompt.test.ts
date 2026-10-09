import { describe, it, expect } from 'vitest'
import { renderAria, OUTPUT_SCHEMA, parseVerdicts } from './prompt'

describe('renderAria', () => {
  it('renders roles, names, state tokens, and text leaves as an indented outline', () => {
    const out = renderAria({
      role: 'root',
      children: [
        { role: 'button', name: 'Previous contact', disabled: true },
        { role: 'paragraph', children: [{ role: 'text', text: 'Contacts merged successfully!' }] },
      ],
    })
    expect(out).toContain('- button "Previous contact" [disabled]')
    expect(out).toContain('- text: Contacts merged successfully!')
  })
})

describe('OUTPUT_SCHEMA', () => {
  it('constrains verdicts to the categorical enum', () => {
    const enumVals = OUTPUT_SCHEMA.properties.verdicts.items.properties.verdict.enum
    expect(enumVals).toEqual(['pass', 'fail', 'unsure'])
  })
})

describe('parseVerdicts', () => {
  it('parses a schema-constrained message', () => {
    const raw = JSON.stringify({
      verdicts: [{ item_index: 0, verdict: 'fail', citation: 'dialog message', critique: 'warns' }],
    })
    expect(parseVerdicts(raw)).toEqual([
      { itemIndex: 0, verdict: 'fail', citation: 'dialog message', critique: 'warns' },
    ])
  })

  it('coerces an unknown verdict to unsure and defaults missing fields', () => {
    const raw = JSON.stringify({ verdicts: [{ item_index: 1, verdict: 'maybe' }] })
    expect(parseVerdicts(raw)).toEqual([
      { itemIndex: 1, verdict: 'unsure', citation: '', critique: '' },
    ])
  })

  it('extracts JSON wrapped in prose/fences', () => {
    const raw =
      'Here is my answer:\n```json\n{"verdicts":[{"item_index":0,"verdict":"pass","citation":"x","critique":"y"}]}\n```'
    expect(parseVerdicts(raw)).toEqual([
      { itemIndex: 0, verdict: 'pass', citation: 'x', critique: 'y' },
    ])
  })

  it('returns [] on unparseable output', () => {
    expect(parseVerdicts('not json at all')).toEqual([])
  })
})
