import { describe, it, expect } from 'vitest'
import { apiItem, cap, frame, root } from './fixtures'
import type { CaptureSet } from './types'
import { con045, expectedProximitySections } from './verifiers/con045'
import type { Capture } from '../../support/types'

function set(behaviorId: string, captures: Capture[]): CaptureSet {
  return { behaviorId, captures }
}

// --- CON-045 ---
describe('con045', () => {
  const birthdays = (
    over: { gift?: boolean; placeholderAged?: boolean; month?: number } = {}
  ): Capture => {
    const month = over.month ?? 6 // July
    const cards = [
      { role: 'heading' as const, name: 'Upcoming Birthdays (2)', level: 2 },
      { role: 'heading' as const, name: 'synth-a', level: 3 },
      { role: 'text' as const, text: 'Turning 30' },
      { role: 'text' as const, text: '3 days' },
      { role: 'heading' as const, name: 'synth-placeholder', level: 3 },
      ...(over.placeholderAged ? [{ role: 'text' as const, text: 'Turning 40' }] : []),
      { role: 'text' as const, text: '7 days' },
      { role: 'heading' as const, name: 'Already Celebrated This Year (1)', level: 2 },
      { role: 'heading' as const, name: 'synth-b', level: 3 },
    ]
    const giftSection = over.gift
      ? [{ role: 'heading' as const, name: 'Gift Planning - Early 2027 Birthdays (1)', level: 2 }]
      : []
    return cap({
      behaviors: ['CON-045'],
      note: 'birthdays page',
      serverTime: frame({ currentTime: `2026-${String(month + 1).padStart(2, '0')}-12T15:48:12Z` }),
      apiResponses: {
        'GET /api/v1/contacts': [
          apiItem({
            query: { limit: '1000' },
            body: {
              data: [
                { full_name: 'synth-a', birthday: '1990-07-15' },
                { full_name: 'synth-placeholder', birthday: '1900-07-19' },
              ],
            },
          }),
        ],
      },
      aria: root([{ role: 'text', text: 'Sunday, July 12, 2026' }, ...giftSection, ...cards]),
    })
  }

  it('clean (July): [0] pass, [1] gift hidden pass, [2] pass, [3] placeholder no age pass, [4] pass', () => {
    const v = con045(set('CON-045', [birthdays()]))
    expect(v[0].verdict).toBe('pass')
    expect(v[1].verdict).toBe('pass') // gift correctly hidden away from year end
    expect(v[2].verdict).toBe('pass')
    expect(v[3].verdict).toBe('pass')
    expect(v[4].verdict).toBe('pass')
  })

  it('[1] fail: gift-planning shown in July (not near year end)', () => {
    const v = con045(set('CON-045', [birthdays({ gift: true })]))
    expect(v[1].verdict).toBe('fail')
  })

  // A December frame with a Jan-Mar (gift-planning) birthday present.
  const december = (over: { gift?: boolean; hasJan?: boolean } = {}): Capture =>
    cap({
      behaviors: ['CON-045'],
      note: 'birthdays page',
      serverTime: frame({ currentTime: '2026-12-15T12:00:00Z' }),
      apiResponses: {
        'GET /api/v1/contacts': [
          apiItem({
            query: { limit: '1000' },
            body: {
              data: [
                {
                  full_name: 'synth-jan',
                  birthday: over.hasJan === false ? '1990-07-15' : '1990-01-20',
                },
              ],
            },
          }),
        ],
      },
      aria: root([
        { role: 'text', text: 'Tuesday, December 15, 2026' },
        ...(over.gift
          ? [
              {
                role: 'heading' as const,
                name: 'Gift Planning - Early 2027 Birthdays (1)',
                level: 2,
              },
            ]
          : []),
        { role: 'heading' as const, name: 'Already Celebrated This Year (1)', level: 2 },
        { role: 'heading' as const, name: 'synth-jan', level: 3 },
      ]),
    })

  it('[1] FAIL near year-end: Jan birthday present but the gift-planning heading is absent', () => {
    expect(con045(set('CON-045', [december({ gift: false })]))[1].verdict).toBe('fail')
  })

  it('[1] pass near year-end: gift-planning heading present', () => {
    expect(con045(set('CON-045', [december({ gift: true })]))[1].verdict).toBe('pass')
  })

  it('[1] pass near year-end: no Jan-Mar birthdays → section correctly absent', () => {
    expect(con045(set('CON-045', [december({ hasJan: false })]))[1].verdict).toBe('pass')
  })

  it('[1] FAIL near year-end: gift-planning heading shown but no Jan-Mar candidates (spurious)', () => {
    // Heading present + list has no early-next-year candidate = spurious render.
    expect(con045(set('CON-045', [december({ gift: true, hasJan: false })]))[1].verdict).toBe(
      'fail'
    )
  })

  it('[1] UNSURE near year-end: gift heading shown but the full list is absent (no evidence)', () => {
    // Heading present near year-end but NO limit=1000 list to confirm candidates
    // → abstain, never pass on missing evidence.
    const noList = cap({
      behaviors: ['CON-045'],
      note: 'birthdays page',
      serverTime: frame({ currentTime: '2026-12-15T12:00:00Z' }),
      apiResponses: {},
      aria: root([
        { role: 'heading', name: 'Gift Planning - Early 2027 Birthdays (1)', level: 2 },
        { role: 'heading', name: 'Upcoming Birthdays (0)', level: 2 },
      ]),
    })
    expect(con045(set('CON-045', [noList]))[1].verdict).toBe('unsure')
  })

  it('[3] fail: a placeholder-year birthday shows an age', () => {
    const v = con045(set('CON-045', [birthdays({ placeholderAged: true })]))
    expect(v[3].verdict).toBe('fail')
  })

  it('[0] fail: a data-expected section heading is missing (page regressed to fewer sections)', () => {
    // A celebrated birthday (Jan, md < July) is present in the list, but the
    // aria carries no "Already Celebrated" heading → the expected section is
    // missing → fail (not a lenient any-section pass).
    const missingCelebrated = cap({
      behaviors: ['CON-045'],
      note: 'birthdays page',
      serverTime: frame({ currentTime: '2026-07-12T15:48:12Z' }),
      apiResponses: {
        'GET /api/v1/contacts': [
          apiItem({
            query: { limit: '1000' },
            body: { data: [{ full_name: 'synth-c', birthday: '1990-01-15' }] },
          }),
        ],
      },
      aria: root([
        { role: 'text', text: 'Sunday, July 12, 2026' },
        { role: 'heading', name: 'Upcoming Birthdays (0)', level: 2 },
      ]),
    })
    expect(con045(set('CON-045', [missingCelebrated]))[0].verdict).toBe('fail')
  })

  // DECISION 1: the verifier reads the birthday projection from
  // fields.birthdayContacts (no full API body needed).
  it('reads the birthday ground-truth from fields.birthdayContacts (no API body)', () => {
    const c = cap({
      behaviors: ['CON-045'],
      note: 'birthdays page',
      serverTime: frame({ currentTime: '2026-07-12T15:48:12Z' }),
      fields: {
        birthdayContacts: [
          { full_name: 'synth-a', birthday: '1990-07-15' },
          { full_name: 'synth-b', birthday: '1990-01-10' },
        ],
      },
      aria: root([
        { role: 'text', text: 'Sunday, July 12, 2026' },
        { role: 'heading', name: 'Upcoming Birthdays (1)', level: 2 },
        { role: 'heading', name: 'synth-a', level: 3 },
        { role: 'text', text: '3 days' },
        { role: 'heading', name: 'Already Celebrated This Year (1)', level: 2 },
        { role: 'heading', name: 'synth-b', level: 3 },
      ]),
    })
    const v = con045(set('CON-045', [c]))
    expect(v[0].verdict).toBe('pass') // upcoming + celebrated both expected & present
    expect(v[4].verdict).toBe('pass')
  })

  it('[3] abstains when a placeholder is in the list but no card was rendered', () => {
    const c = cap({
      behaviors: ['CON-045'],
      note: 'birthdays page',
      serverTime: frame({ currentTime: '2026-07-12T15:48:12Z' }),
      fields: { birthdayContacts: [{ full_name: 'synth-placeholder', birthday: '1900-07-19' }] },
      aria: root([
        { role: 'text', text: 'Sunday, July 12, 2026' },
        { role: 'heading', name: 'Upcoming Birthdays (0)', level: 2 },
      ]),
    })
    expect(con045(set('CON-045', [c]))[3].verdict).toBe('unsure')
  })

  it('UTC-lexical: a T..Z-suffixed birthday does not shift the group', () => {
    // A local-TZ parse of 1990-07-12T23:00:00Z could roll to 07-13; lexical
    // extraction keeps 07-12 → the "today" group for a 07-12 frame.
    const s = expectedProximitySections(
      [{ birthday: '1990-07-12T23:00:00Z' }],
      '2026-07-12T00:00:00Z'
    )
    expect([...(s ?? [])]).toEqual(['today'])
  })

  it('expectedProximitySections computes the non-empty groups from the frame', () => {
    const contacts = [
      { birthday: '1990-07-12' }, // today (same month/day as the frame)
      { birthday: '1990-08-01' }, // upcoming (later this year)
      { birthday: '1990-01-15' }, // already celebrated (earlier this year)
    ]
    const s = expectedProximitySections(contacts, '2026-07-12T00:00:00Z')
    expect([...(s ?? [])].sort()).toEqual(['celebrated', 'today', 'upcoming'])
    expect(expectedProximitySections([], '2026-07-12T00:00:00Z')).toBeUndefined()
  })

  it('missing capture → unsure across all items', () => {
    const v = con045(set('CON-045', []))
    for (const i of [0, 1, 2, 3, 4]) expect(v[i].verdict).toBe('unsure')
  })
})
