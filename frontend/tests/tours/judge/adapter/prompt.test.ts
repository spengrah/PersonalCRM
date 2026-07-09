import { describe, it, expect } from 'vitest'
import { BLOCK_ORDER, buildPrompt, OUTPUT_SCHEMA, parseVerdicts, renderAria } from './prompt'
import type { JudgeInput } from './types'

const input: JudgeInput = {
  behaviorId: 'CON-042',
  behaviorTitle: 'Deleting a contact requires explicit confirmation',
  given: 'a contact detail page',
  when: 'the user asks to delete the contact',
  then: [
    'a confirmation prompt warns the action cannot be undone',
    'only on confirmation is the contact deleted',
  ],
  items: [{ itemIndex: 0, thenText: 'a confirmation prompt warns the action cannot be undone' }],
  evidence: {
    url: '/contacts/<id:1>',
    aria: { role: 'root', children: [{ role: 'button', name: 'Delete' }] },
    serverTime: {
      currentTime: '2026-07-12T00:00:00Z',
      isAccelerated: true,
      accelerationFactor: 60,
      baseTime: '2026-07-09T00:00:00Z',
    },
    dialogs: [{ type: 'confirm', message: 'Are you sure? This action cannot be undone.' }],
  },
}

describe('buildPrompt', () => {
  it('emits the fixed labeled-block order and the residual item', () => {
    const p = buildPrompt(input)
    // SPEC before URL before ARIA before SERVER_TIME before DIALOGS before ITEMS.
    const positions = ['SPEC', 'URL', 'ARIA', 'SERVER_TIME', 'DIALOGS', 'ITEMS'].map(b =>
      p.indexOf(`=== ${b} ===`)
    )
    expect(positions.every(x => x >= 0)).toBe(true)
    for (let i = 1; i < positions.length; i++)
      expect(positions[i]).toBeGreaterThan(positions[i - 1])
    expect(p).toContain('[0] a confirmation prompt warns the action cannot be undone')
    expect(p).toContain('This action cannot be undone')
  })

  it('instructs no-tools and the grounding rule', () => {
    const p = buildPrompt(input)
    expect(p).toMatch(/Do NOT use any tool/)
    expect(p).toMatch(/GROUNDING RULE/)
  })

  it('omits absent evidence blocks (API not present here)', () => {
    expect(buildPrompt(input)).not.toContain('=== API ===')
  })

  it('BLOCK_ORDER documents the canonical order', () => {
    expect(BLOCK_ORDER).toEqual(['SPEC', 'URL', 'ARIA', 'API', 'SERVER_TIME', 'DIALOGS', 'ITEMS'])
  })
})

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
