import { describe, it, expect } from 'vitest'
import { applyMutation } from './doctor'
import { apiItem, cap, pair, root } from './grader/fixtures'
import type { Mutation } from './corpus/schema'

describe('applyMutation — single-point, deterministic, non-mutating', () => {
  it('inject_query re-injects an action= param', () => {
    const base = [cap({ behaviors: ['CON-041'], url: '/contacts/<id:1>?sort=cadence&order=desc' })]
    const m: Mutation = { op: 'inject_query', param: 'action', value: 'edit' }
    const out = applyMutation(base, m)
    expect(out[0].url).toBe('/contacts/<id:1>?sort=cadence&order=desc&action=edit')
    expect(base[0].url).toBe('/contacts/<id:1>?sort=cadence&order=desc') // input untouched
  })

  it('delete_endpoint removes the POST interactions item', () => {
    const base = [
      cap({
        behaviors: ['CON-044'],
        pair: pair('mc', 'after'),
        apiResponses: {
          'POST /api/v1/contacts/:id/interactions': [apiItem({ method: 'POST', status: 201 })],
        },
      }),
    ]
    const out = applyMutation(base, {
      op: 'delete_endpoint',
      endpoint: 'POST /api/v1/contacts/:id/interactions',
    })
    expect(out[0].apiResponses['POST /api/v1/contacts/:id/interactions']).toBeUndefined()
  })

  it('set_aria_disabled flips the Previous nav at the boundary', () => {
    const base = [
      cap({
        behaviors: ['CON-040'],
        pair: pair('k', 'boundary-first'),
        aria: root([{ role: 'button', name: 'Previous contact', disabled: true }]),
      }),
    ]
    const out = applyMutation(base, {
      op: 'set_aria_disabled',
      node_role: 'button',
      node_name: 'Previous contact',
      value: false,
    })
    const btn = out[0].aria.children![0] as { disabled?: boolean }
    expect(btn.disabled).toBe(false)
  })

  it('reorder_ids swaps the first two ids_only ids', () => {
    const base = [
      cap({
        behaviors: ['CON-038'],
        pair: pair('d', 'detail'),
        apiResponses: {
          'GET /api/v1/contacts': [
            apiItem({
              query: { ids_only: 'true' },
              body: { data: { ids: ['<id:1>', '<id:2>', '<id:3>'], total: 3 } },
            }),
          ],
        },
      }),
    ]
    const out = applyMutation(base, { op: 'reorder_ids', mode: 'swap-first-two' })
    const ids = (out[0].apiResponses['GET /api/v1/contacts'][0].body as { data: { ids: string[] } })
      .data.ids
    expect(ids).toEqual(['<id:2>', '<id:1>', '<id:3>'])
  })

  it('blank_dialog empties the dialog message', () => {
    const base = [
      cap({ behaviors: ['CON-042'], dialogs: [{ type: 'confirm', message: 'cannot be undone' }] }),
    ]
    const out = applyMutation(base, { op: 'blank_dialog' })
    expect(out[0].dialogs[0].message).toBe('')
  })

  it('is byte-stable across two runs', () => {
    const base = [cap({ behaviors: ['CON-041'], url: '/contacts/<id:1>' })]
    const m: Mutation = { op: 'inject_query', param: 'action', value: 'edit' }
    expect(JSON.stringify(applyMutation(base, m))).toBe(JSON.stringify(applyMutation(base, m)))
  })
})
