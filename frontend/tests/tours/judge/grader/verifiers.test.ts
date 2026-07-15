import { describe, it, expect } from 'vitest'
import { apiItem, cap, frame, pair, root } from './fixtures'
import type { CaptureSet } from './types'
import { con040 } from './verifiers/con040'
import { con041 } from './verifiers/con041'
import { con042 } from './verifiers/con042'
import { con043 } from './verifiers/con043'
import { con044 } from './verifiers/con044'
import { con045, expectedProximitySections } from './verifiers/con045'
import type { Capture } from '../../support/types'

function set(behaviorId: string, captures: Capture[]): CaptureSet {
  return { behaviorId, captures }
}

// --- CON-040 ---
describe('con040', () => {
  const build = (over: { prevDisabled?: boolean; editInertUrl?: string } = {}): Capture[] => [
    cap({ behaviors: ['CON-040'], pair: pair('k', 'view-before'), url: '/contacts/<id:2>' }),
    cap({ behaviors: ['CON-040'], pair: pair('k', 'arrow-right-next'), url: '/contacts/<id:3>' }),
    cap({ behaviors: ['CON-040'], pair: pair('k', 'arrow-left-prev'), url: '/contacts/<id:2>' }),
    cap({
      behaviors: ['CON-040'],
      pair: pair('k', 'boundary-first'),
      url: '/contacts/<id:1>',
      aria: root([
        {
          role: 'button',
          name: 'Previous contact',
          ...(over.prevDisabled === false ? {} : { disabled: true }),
        },
        { role: 'button', name: 'Next contact' },
      ]),
    }),
    cap({ behaviors: ['CON-040'], pair: pair('k', 'input-focus-inert'), url: '/contacts/<id:1>' }),
    cap({
      behaviors: ['CON-040'],
      pair: pair('k', 'enter-edit'),
      url: '/contacts/<id:2>',
      aria: root([{ role: 'heading', name: 'Edit Contact', level: 2 }]),
    }),
    cap({
      behaviors: ['CON-040'],
      pair: pair('k', 'arrow-edit-inert'),
      url: over.editInertUrl ?? '/contacts/<id:2>',
    }),
    cap({
      behaviors: ['CON-040'],
      pair: pair('k', 'escape-discard'),
      aria: root([{ role: 'button', name: 'Edit' }]),
    }),
    cap({
      behaviors: ['CON-040'],
      pair: pair('k', 'escape-to-list'),
      url: '/contacts?sort=cadence&order=desc',
    }),
  ]

  it('renamed Edit Contact heading after Enter → [2] unbound (routed to the judge)', () => {
    const caps = build()
    const idx = caps.findIndex(c => c.pair?.role === 'enter-edit')
    caps[idx] = {
      ...caps[idx],
      aria: root([{ role: 'heading', name: 'Update Contact', level: 2 }]),
    }
    expect(con040(set('CON-040', caps))[2].verdict).toBe('unbound')
  })

  it('clean: [0] abstains (last-boundary caveat), [1]/[2]/[3] pass', () => {
    const v = con040(set('CON-040', build()))
    expect(v[0].verdict).toBe('unsure')
    expect(v[0].reason).toMatch(/last boundary/)
    expect(v[1].verdict).toBe('pass')
    expect(v[2].verdict).toBe('pass')
    expect(v[3].verdict).toBe('pass')
  })

  it('doctored: Previous NOT disabled at the first boundary → [0] fail', () => {
    const v = con040(set('CON-040', build({ prevDisabled: false })))
    expect(v[0].verdict).toBe('fail')
  })

  // Follow-up 3: the last-boundary capture (Next disabled at the last contact).
  const boundaryLast = (nextDisabled = true): Capture =>
    cap({
      behaviors: ['CON-040'],
      pair: pair('k', 'boundary-last'),
      url: '/contacts/<id:9>',
      aria: root([
        { role: 'button', name: 'Previous contact' },
        { role: 'button', name: 'Next contact', ...(nextDisabled ? { disabled: true } : {}) },
      ]),
    })

  it('both boundaries captured (Previous + Next disabled) → [0] pass', () => {
    const v = con040(set('CON-040', [...build(), boundaryLast(true)]))
    expect(v[0].verdict).toBe('pass')
  })

  it('doctored: Next NOT disabled at the last boundary → [0] fail', () => {
    const v = con040(set('CON-040', [...build(), boundaryLast(false)]))
    expect(v[0].verdict).toBe('fail')
  })

  it('doctored: edit-mode arrow changed the url → [1] fail', () => {
    const v = con040(set('CON-040', build({ editInertUrl: '/contacts/<id:99>' })))
    expect(v[1].verdict).toBe('fail')
  })

  it('[1] abstains (unsure) when one inert bracket is missing — never a lone-half pass', () => {
    // Drop the input-focus-inert capture: the edit-mode half passes, but the
    // item must abstain, not pass on a single half.
    const caps = build().filter(c => c.pair?.role !== 'input-focus-inert')
    const v = con040(set('CON-040', caps))
    expect(v[1].verdict).toBe('unsure')
  })

  it('missing evidence → unsure', () => {
    const v = con040(set('CON-040', []))
    for (const i of [0, 1, 2, 3]) expect(v[i].verdict).toBe('unsure')
  })
})

// --- CON-041 ---
describe('con041', () => {
  const build = (editUrl = '/contacts/<id:1>?sort=cadence&order=desc'): Capture[] => [
    cap({
      behaviors: ['CON-041'],
      note: 'action=edit consumed once and stripped from URL',
      url: editUrl,
      aria: root([{ role: 'heading', name: 'Edit Contact', level: 2 }]),
    }),
    cap({
      behaviors: ['CON-041'],
      note: 'action=merge consumed once and stripped from URL',
      url: '/contacts/<id:1>?sort=cadence&order=desc',
      aria: root([{ role: 'heading', name: 'Merge Contacts', level: 2 }]),
    }),
  ]

  it('clean: [0] and [1] pass', () => {
    const v = con041(set('CON-041', build()))
    expect(v[0].verdict).toBe('pass')
    expect(v[1].verdict).toBe('pass')
  })

  it('doctored: re-injected action=edit → [1] fail', () => {
    const v = con041(set('CON-041', build('/contacts/<id:1>?sort=cadence&order=desc&action=edit')))
    expect(v[1].verdict).toBe('fail')
  })

  it('renamed surface heading → [0] unbound (routed to the judge, never fail)', () => {
    const caps = build()
    const renamed = [
      { ...caps[0], aria: root([{ role: 'heading', name: 'Update Contact', level: 2 }]) },
      caps[1],
    ]
    expect(con041(set('CON-041', renamed))[0].verdict).toBe('unbound')
  })

  it('missing evidence → unsure', () => {
    const v = con041(set('CON-041', []))
    expect(v[0].verdict).toBe('unsure')
    expect(v[1].verdict).toBe('unsure')
  })
})

// --- CON-042 ---
describe('con042', () => {
  const build = (acceptProbeStatus = 404): Capture[] => [
    cap({
      behaviors: ['CON-042'],
      pair: pair('del', 'after-dismiss'),
      url: '/contacts/<id:2>?sort=cadence&order=desc',
      apiResponses: {
        'GET /api/v1/contacts/:id': [apiItem({ method: 'GET', status: 200, probe: true })],
      },
    }),
    cap({
      behaviors: ['CON-042'],
      pair: pair('del', 'after-accept'),
      url: '/contacts',
      apiResponses: {
        'DELETE /api/v1/contacts/:id': [apiItem({ method: 'DELETE', status: 204 })],
        'GET /api/v1/contacts/:id': [
          apiItem({ method: 'GET', status: acceptProbeStatus, probe: true }),
        ],
      },
    }),
  ]

  it('clean: [1] pass (dismiss live, accept deleted), [2] pass (back to list)', () => {
    const v = con042(set('CON-042', build()))
    expect(v[1].verdict).toBe('pass')
    expect(v[2].verdict).toBe('pass')
  })

  it('doctored: confirmed delete probe still 200 → [1] fail', () => {
    const v = con042(set('CON-042', build(200)))
    expect(v[1].verdict).toBe('fail')
  })

  it('[1] fails when the after-accept bracket is PRESENT but its DELETE/404 evidence is absent', () => {
    // A present bracket missing its required mutation is a fail, not unsure.
    const emptyAccept = cap({
      behaviors: ['CON-042'],
      pair: pair('del', 'after-accept'),
      url: '/contacts',
    })
    const v = con042(set('CON-042', [emptyAccept]))
    expect(v[1].verdict).toBe('fail')
  })

  it('missing evidence (no brackets) → unsure', () => {
    const v = con042(set('CON-042', []))
    expect(v[1].verdict).toBe('unsure')
    expect(v[2].verdict).toBe('unsure')
  })
})

// --- CON-044 ---
describe('con044', () => {
  const afterCap = (withPost: boolean): Capture =>
    cap({
      behaviors: ['CON-044'],
      pair: pair('mc', 'after'),
      apiResponses: withPost
        ? {
            'POST /api/v1/contacts/:id/interactions': [
              apiItem({
                method: 'POST',
                status: 201,
                requestBody: { direction: 'mutual' },
                body: { data: { direction: 'mutual', occurred_at: '2026-07-12T16:14:34Z' } },
              }),
            ],
          }
        : {},
    })

  it('clean: [0] pass (mutual, server-timestamped)', () => {
    expect(con044(set('CON-044', [afterCap(true)]))[0].verdict).toBe('pass')
  })

  it('doctored: POST interaction deleted from the present after bracket → [0] fail', () => {
    expect(con044(set('CON-044', [afterCap(false)]))[0].verdict).toBe('fail')
  })

  it('missing after bracket entirely → unsure', () => {
    expect(con044(set('CON-044', []))[0].verdict).toBe('unsure')
  })
})

// --- CON-043 ---
describe('con043', () => {
  const openCap = cap({
    behaviors: ['CON-043'],
    pair: pair('m', 'open'),
    aria: root([
      { role: 'heading', name: 'Merge Contacts', level: 2 },
      { role: 'heading', name: 'synth-target', level: 3 },
      { role: 'button', name: 'Merge Contacts', disabled: true },
    ]),
  })
  const selectorCap = cap({
    behaviors: ['CON-043'],
    pair: pair('m', 'selector-open'),
    aria: root([{ role: 'option', name: 'synth-other' }]),
  })
  const previewCap = cap({
    behaviors: ['CON-043'],
    pair: pair('m', 'preview-loaded'),
    apiResponses: {
      'GET /api/v1/contacts/:id/merge/preview': [apiItem({ status: 200, body: { data: {} } })],
    },
    aria: root([
      { role: 'text', text: 'Keeping Merge from Archiving synth-other' },
      { role: 'heading', name: 'Resolve Conflicts', level: 3 },
      { role: 'heading', name: 'Will Be Merged', level: 3 },
      { role: 'button', name: 'use this' },
    ]),
  })
  const previewLoadingCap = cap({
    behaviors: ['CON-043'],
    pair: pair('m', 'preview-loading'),
    aria: root([{ role: 'button', name: 'Merge Contacts', disabled: true }]),
  })
  const quickfilledCap = cap({
    behaviors: ['CON-043'],
    pair: pair('m', 'name-quickfilled'),
    aria: root([{ role: 'button', name: 'use this' }]),
  })
  const nameEditedCap = cap({
    behaviors: ['CON-043'],
    pair: pair('m', 'name-edited'),
    fields: { mergedNameInput: 'synth-other (merged)' },
  })
  const inFlightCap = cap({
    behaviors: ['CON-043'],
    pair: pair('m', 'in-flight'),
    aria: root([{ role: 'button', name: 'Merge Contacts', disabled: true }]),
  })
  const afterCap = (selections: Record<string, string>): Capture =>
    cap({
      behaviors: ['CON-043'],
      pair: pair('m', 'after'),
      apiResponses: {
        'POST /api/v1/contacts/:id/merge': [
          apiItem({ method: 'POST', status: 200, requestBody: { field_selections: selections } }),
        ],
      },
    })
  const outcomeCap = cap({
    behaviors: ['CON-043'],
    pair: pair('m', 'outcome-reported'),
    aria: root([{ role: 'text', text: 'Contacts merged successfully!' }]),
  })
  const dismissedCap = cap({ behaviors: ['CON-043'], pair: pair('m', 'dismissed'), aria: root([]) })

  it('renamed copy anchors → unbound, never fail: Keeping badge / Will Be Merged / use this / banner', () => {
    // [0] preview + open present but no 'Keeping' text anywhere.
    const noKeeping = {
      ...previewCap,
      aria: root([
        { role: 'heading', name: 'Resolve Conflicts', level: 3 },
        { role: 'heading', name: 'Will Be Merged', level: 3 },
      ]),
    }
    expect(con043(set('CON-043', [openCap, noKeeping]))[0].verdict).toBe('unbound')

    // [1] preview REQUEST present but the 'Will Be Merged' heading renamed.
    const renamedPreview = {
      ...previewCap,
      aria: root([{ role: 'heading', name: 'To Be Combined', level: 3 }]),
    }
    expect(con043(set('CON-043', [renamedPreview]))[1].verdict).toBe('unbound')

    // [1] preview REQUEST missing entirely stays a structural FAIL.
    const noRequest = { ...previewCap, apiResponses: {} }
    expect(con043(set('CON-043', [noRequest]))[1].verdict).toBe('fail')

    // [3] 'use this' quick-fill renamed in BOTH carrying captures (a single
    // absent side leaves the disjunction undefined → unsure, not unbound).
    const renamedQuickfill = {
      ...quickfilledCap,
      aria: root([{ role: 'button', name: 'adopt name' }]),
    }
    const previewNoQuickfill = {
      ...previewCap,
      aria: root([{ role: 'heading', name: 'Will Be Merged', level: 3 }]),
    }
    expect(con043(set('CON-043', [renamedQuickfill, previewNoQuickfill]))[3].verdict).toBe(
      'unbound'
    )

    // [5] success banner not bound in the captured aria.
    const noBanner = { ...outcomeCap, aria: root([{ role: 'text', text: 'done' }]) }
    expect(con043(set('CON-043', [noBanner]))[5].verdict).toBe('unbound')
  })

  const clean = (): Capture[] => [
    openCap,
    selectorCap,
    previewLoadingCap,
    previewCap,
    quickfilledCap,
    nameEditedCap,
    inFlightCap,
    afterCap({ cadence: 'target', location: 'target', birthday: 'target' }),
    outcomeCap,
    dismissedCap,
  ]

  it('clean: all six items pass', () => {
    const v = con043(set('CON-043', clean()))
    for (const i of [0, 1, 2, 3, 4, 5]) expect(v[i].verdict, `item ${i}`).toBe('pass')
  })

  it('doctored: field_selections not all target → [2] fail', () => {
    const caps = clean()
    caps[7] = afterCap({ cadence: 'source', location: 'target', birthday: 'target' })
    expect(con043(set('CON-043', caps))[2].verdict).toBe('fail')
  })

  it('[4] abstains (unsure) when the in-flight submit is not aria-disabled (spinner-based)', () => {
    const caps = clean()
    // in-flight capture present but the submit is NOT aria-[disabled].
    caps[6] = cap({
      behaviors: ['CON-043'],
      pair: pair('m', 'in-flight'),
      aria: root([{ role: 'button', name: 'Merge Contacts' }]),
    })
    expect(con043(set('CON-043', caps))[4].verdict).toBe('unsure')
  })

  it('[4] fails when the before-source submit is enabled', () => {
    const caps = clean()
    caps[0] = cap({
      behaviors: ['CON-043'],
      pair: pair('m', 'open'),
      aria: root([
        { role: 'heading', name: 'synth-target', level: 3 },
        { role: 'button', name: 'Merge Contacts' },
      ]),
    })
    expect(con043(set('CON-043', caps))[4].verdict).toBe('fail')
  })

  it('doctored: target appears in the selector → [0] fail', () => {
    const caps = clean()
    caps[1] = cap({
      behaviors: ['CON-043'],
      pair: pair('m', 'selector-open'),
      aria: root([{ role: 'option', name: 'synth-target' }]),
    })
    expect(con043(set('CON-043', caps))[0].verdict).toBe('fail')
  })

  it('missing evidence → unsure', () => {
    const v = con043(set('CON-043', []))
    for (const i of [0, 1, 2, 3, 4, 5]) expect(v[i].verdict).toBe('unsure')
  })
})

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
