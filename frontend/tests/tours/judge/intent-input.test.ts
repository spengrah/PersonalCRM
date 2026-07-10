// Binding: an intent's evidence = captures tagged with the intent ID or any
// servedBy behavior, deduped by (tour, seq), ordered, capped with an explicit
// dropped count. Zero-bind stays empty (the runner abstains without a call).

import { describe, expect, it } from 'vitest'
import type { Capture } from '../support/types'
import type { IntentSpec } from './intent-catalog'
import { bindIntentCaptures, buildIntentJudgeInput } from './intent-input'

function cap(tour: string, seq: number, behaviors: string[]): Capture {
  return {
    captureFormatVersion: 1,
    captureGeneratorVersion: 1,
    tour,
    seq,
    behaviors,
    note: `note-${tour}-${seq}`,
    url: `http://x/${tour}/${seq}`,
    pair: null,
    serverTime: {
      currentTime: 't',
      isAccelerated: true,
      accelerationFactor: 1,
      baseTime: 't',
    },
    aria: { role: 'root', children: [] },
    apiResponses: {},
    dialogs: [],
  }
}

const intent: IntentSpec = {
  id: 'DSH-010',
  title: 't',
  statement: 's',
  status: 'current',
  servedBy: ['CAD-026', 'CAD-027'],
}

describe('bindIntentCaptures', () => {
  it('binds servedBy-tagged and directly-tagged captures, skips others', () => {
    const all = [
      cap('dashboard', 1, ['CAD-026']),
      cap('dashboard', 2, ['DSH-002']), // unrelated
      cap('dashboard', 3, ['DSH-010']), // direct intent tag
      cap('contacts', 1, ['CAD-027', 'CON-038']),
    ]
    const { captures, dropped } = bindIntentCaptures(intent, all)
    expect(captures.map(c => `${c.tour}#${c.seq}`)).toEqual([
      'contacts#1',
      'dashboard#1',
      'dashboard#3',
    ])
    expect(dropped).toBe(0)
  })

  it('dedupes a capture tagging several bound behaviors', () => {
    const all = [cap('dashboard', 1, ['CAD-026', 'CAD-027', 'DSH-010'])]
    expect(bindIntentCaptures(intent, all).captures).toHaveLength(1)
  })

  it('caps and reports the dropped count', () => {
    const all = Array.from({ length: 5 }, (_, i) => cap('dashboard', i, ['CAD-026']))
    const { captures, dropped } = bindIntentCaptures(intent, all, 3)
    expect(captures.map(c => c.seq)).toEqual([0, 1, 2])
    expect(dropped).toBe(2)
  })

  it('binds nothing when no capture tags the intent or a serving behavior', () => {
    const { captures, dropped } = bindIntentCaptures(intent, [cap('dashboard', 1, ['DSH-002'])])
    expect(captures).toHaveLength(0)
    expect(dropped).toBe(0)
  })
})

describe('buildIntentJudgeInput', () => {
  it('builds the single-item intent variant with per-capture sections', () => {
    const bound = [cap('dashboard', 1, ['CAD-026']), cap('dashboard', 2, ['CAD-027'])]
    const input = buildIntentJudgeInput(intent, bound)
    expect(input.behaviorId).toBe('DSH-010')
    expect(input.intent).toEqual({ statement: 's', status: 'current' })
    expect(input.items).toEqual([{ itemIndex: 0, thenText: 's' }])
    expect(input.captureSections).toHaveLength(2)
    expect(input.captureSections?.[0].note).toBe('dashboard#1 — note-dashboard-1')
    expect(input.captureSections?.[0].evidence.url).toBe('http://x/dashboard/1')
    expect(input.images).toBeUndefined()
  })

  it('attaches screenshots only when EVERY bound capture resolves (order preserved)', () => {
    const bound = [cap('dashboard', 1, ['CAD-026']), cap('dashboard', 2, ['CAD-027'])]
    const input = buildIntentJudgeInput(intent, bound, c => `/runs/x/${c.seq}.png`)
    expect(input.images).toEqual(['/runs/x/1.png', '/runs/x/2.png'])
  })

  it('drops ALL images on a gap — a partial set would misalign CAPTURE[n]', () => {
    const bound = [cap('dashboard', 1, ['CAD-026']), cap('dashboard', 2, ['CAD-027'])]
    const input = buildIntentJudgeInput(intent, bound, c =>
      c.seq === 2 ? '/runs/x/2.png' : undefined
    )
    expect(input.images).toBeUndefined()
  })
})
