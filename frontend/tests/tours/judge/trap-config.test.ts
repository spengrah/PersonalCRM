import { describe, it, expect } from 'vitest'
import { TRAPS } from './trap-config'
import { type Mutation, parseMutation } from './mutation'
import { applyMutation } from './doctor'
import { judgeItemsFor } from './judge-input'
import { apiItem, cap, root } from './grader/fixtures'

// The judge lane renders url/aria/api/serverTime/dialogs but NOT `Capture.fields`
// (buildEvidence/captureSection omit it), so a `set_field` mutation is invisible
// to the judge and would silently no-op. Only these ops reach the graded prompt.
const JUDGE_PROJECTED_OPS = new Set(['blank_dialog', 'set_json_field', 'remove_aria_subtree'])

describe('TRAPS — the committed trap config (D2 contract)', () => {
  it('holds 2–3 traps (bounds per-round judge quota)', () => {
    expect(TRAPS.length).toBeGreaterThanOrEqual(2)
    expect(TRAPS.length).toBeLessThanOrEqual(3)
  })

  it('every trap has a unique id', () => {
    expect(new Set(TRAPS.map(t => t.id)).size).toBe(TRAPS.length)
  })

  it('every mutation is a valid Mutation', () => {
    for (const t of TRAPS) expect(() => parseMutation(t.mutation)).not.toThrow()
  })

  it('every trap targets a JUDGE-GRADED residue item (else the judge short-circuits)', () => {
    for (const t of TRAPS) {
      const items = judgeItemsFor(t.targetBehavior)
      expect(
        items.some(i => i.itemIndex === t.targetItem),
        `${t.id}: ${t.targetBehavior}[${t.targetItem}] is not judge residue`
      ).toBe(true)
    }
  })

  it('uses a JUDGE-PROJECTED op (never set_field, invisible to the judge lane)', () => {
    for (const t of TRAPS) {
      expect(
        JUDGE_PROJECTED_OPS.has(t.mutation.op),
        `${t.id}: op ${t.mutation.op} not projected`
      ).toBe(true)
    }
  })

  it('targets exactly the surviving residue: CON-042[0] + DSH-004[2]', () => {
    const targets = TRAPS.map(t => `${t.targetBehavior}[${t.targetItem}]`).sort()
    expect(targets).toEqual(['CON-042[0]', 'DSH-004[2]'])
  })
})

// JSON-only tripwire (arc R4). `applyMutation` is a pure JSON deep-clone with NO
// fs access — it never dereferences the `screenshot` PATH. This asserts that the
// TRAPS mutations (and a sample of EVERY schema op) leave the screenshot path
// byte-identical, so the `screenshot_caveat`'s "doctoring is JSON-only" claim
// holds: the pixels genuinely show the undoctored world. A TRIPWIRE for a future
// path-touching op, not a proof of byte-immutability — such an op needs review.
describe('JSON-only tripwire — mutations never touch the screenshot path', () => {
  const SCREENSHOT = 'screenshots/dashboard/011-overdue.png'
  const fixture = () => [
    cap({
      behaviors: ['DSH-004'],
      url: '/contacts/<id:1>?sort=cadence',
      screenshot: SCREENSHOT,
      dialogs: [{ type: 'confirm', message: 'This action cannot be undone.' }],
      fields: { overdueLoadingSkeletons: 3 },
      aria: root([{ role: 'link', name: 'Add Contact' }]),
      apiResponses: {
        'GET /api/v1/contacts': [
          apiItem({ query: { ids_only: 'true' }, body: { data: { ids: ['<id:1>', '<id:2>'] } } }),
        ],
        'GET /api/v1/contacts/overdue': [
          apiItem({ status: 500, body: { error: { message: 'overdue fetch failed' } } }),
        ],
      },
    }),
  ]

  // One instance of every schema op — proves the whole schema, not just the two
  // shipped traps, keeps the screenshot untouched.
  const SAMPLE_OPS: Mutation[] = [
    { op: 'inject_query', param: 'action', value: 'edit' },
    { op: 'delete_endpoint', endpoint: 'GET /api/v1/contacts/overdue' },
    { op: 'set_aria_disabled', node_role: 'link', node_name: 'Add Contact', value: true },
    { op: 'reorder_ids', mode: 'swap-first-two' },
    { op: 'blank_dialog' },
    { op: 'remove_aria_subtree', node_role: 'link', node_name: 'Add Contact' },
    { op: 'set_field', field: 'overdueLoadingSkeletons', value: 0 },
    {
      op: 'set_json_field',
      endpoint: 'GET /api/v1/contacts/overdue',
      path: ['error', 'message'],
      value: 'database connection refused',
    },
  ]

  for (const m of [...TRAPS.map(t => t.mutation), ...SAMPLE_OPS]) {
    it(`${m.op} leaves the screenshot path byte-identical`, () => {
      const out = applyMutation(fixture(), m)
      expect(out[0].screenshot).toBe(SCREENSHOT)
    })
  }
})
