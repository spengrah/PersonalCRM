import { describe, it, expect } from 'vitest'
import { applyMutation } from './doctor'
import { apiItem, cap, pair, root } from './grader/fixtures'
import type { Mutation } from './mutation'

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

  it('remove_aria_subtree drops a named node (the Add Contact CTA)', () => {
    const base = [
      cap({
        behaviors: ['DSH-003'],
        aria: root([
          { role: 'heading', name: 'Action Required', level: 2 },
          { role: 'link', name: 'Add Contact' },
        ]),
      }),
    ]
    const out = applyMutation(base, {
      op: 'remove_aria_subtree',
      node_role: 'link',
      node_name: 'Add Contact',
    })
    expect(out[0].aria.children).toEqual([{ role: 'heading', name: 'Action Required', level: 2 }])
    expect(base[0].aria.children).toHaveLength(2) // input untouched
  })

  it('remove_aria_subtree drops a text leaf (the No tasks yet empty state)', () => {
    const base = [
      cap({
        behaviors: ['CAD-030'],
        aria: root([
          { role: 'heading', name: 'Tasks', level: 3 },
          { role: 'text', text: 'No tasks yet' },
        ]),
      }),
    ]
    const out = applyMutation(base, {
      op: 'remove_aria_subtree',
      node_role: 'text',
      node_name: 'No tasks yet',
    })
    expect(out[0].aria.children).toEqual([{ role: 'heading', name: 'Tasks', level: 3 }])
  })

  it('set_field overwrites a fields value (skeleton count)', () => {
    const base = [cap({ behaviors: ['DSH-004'], fields: { overdueLoadingSkeletons: 3 } })]
    const out = applyMutation(base, { op: 'set_field', field: 'overdueLoadingSkeletons', value: 0 })
    expect(out[0].fields?.overdueLoadingSkeletons).toBe(0)
    expect(base[0].fields?.overdueLoadingSkeletons).toBe(3) // input untouched
  })

  it('set_json_field clears a body path (detail last_outreach_at)', () => {
    const base = [
      cap({
        behaviors: ['CAD-029'],
        apiResponses: {
          'GET /api/v1/contacts/:id': [
            apiItem({ body: { data: { last_outreach_at: '2026-07-12T12:00:00Z' } } }),
          ],
        },
      }),
    ]
    const out = applyMutation(base, {
      op: 'set_json_field',
      endpoint: 'GET /api/v1/contacts/:id',
      path: ['data', 'last_outreach_at'],
      value: null,
    })
    const body = out[0].apiResponses['GET /api/v1/contacts/:id'][0].body as {
      data: { last_outreach_at: unknown }
    }
    expect(body.data.last_outreach_at).toBeNull()
  })

  it('set_json_field itemIndex targets a later endpoint item (stale-reason mutation)', () => {
    const base = [
      cap({
        behaviors: ['DSH-004'],
        apiResponses: {
          'GET /api/v1/contacts/overdue': [
            apiItem({ status: 200, body: null }),
            apiItem({ status: 500, body: { error: { message: 'overdue fetch failed' } } }),
          ],
        },
      }),
    ]
    const out = applyMutation(base, {
      op: 'set_json_field',
      endpoint: 'GET /api/v1/contacts/overdue',
      path: ['error', 'message'],
      value: 'database connection lost',
      itemIndex: 1,
    })
    const items = out[0].apiResponses['GET /api/v1/contacts/overdue']
    expect(items[0].body).toBeNull() // untouched (and a null body never throws)
    expect((items[1].body as { error: { message: string } }).error.message).toBe(
      'database connection lost'
    )
  })

  it('set_json_field itemMatch:last-error targets the final 500, skipping a leading 200 + earlier retries', () => {
    const reason = (m: string) => ({ error: { message: m } })
    const base = [
      cap({
        behaviors: ['DSH-004'],
        apiResponses: {
          'GET /api/v1/contacts/overdue': [
            apiItem({ status: 200, body: { data: { overdue: [] } } }),
            apiItem({ status: 500, body: reason('retry-1') }),
            apiItem({ status: 500, body: reason('retry-2') }),
            apiItem({ status: 500, body: reason('retry-3') }),
            apiItem({ status: 500, body: reason('retry-4') }),
          ],
        },
      }),
    ]
    const out = applyMutation(base, {
      op: 'set_json_field',
      endpoint: 'GET /api/v1/contacts/overdue',
      path: ['error', 'message'],
      value: 'database connection refused',
      itemMatch: 'last-error',
    })
    const items = out[0].apiResponses['GET /api/v1/contacts/overdue']
    const msg = (i: number) => (items[i].body as { error: { message: string } }).error.message
    // Only the FINAL 500 (index 4) is rewritten — the penultimate retry a fixed
    // itemIndex:3 would have hit is left intact.
    expect(msg(4)).toBe('database connection refused')
    expect(msg(3)).toBe('retry-3')
    expect(items[0].body).toEqual({ data: { overdue: [] } }) // leading 200 untouched
  })

  it('is byte-stable across two runs', () => {
    const base = [cap({ behaviors: ['CON-041'], url: '/contacts/<id:1>' })]
    const m: Mutation = { op: 'inject_query', param: 'action', value: 'edit' }
    expect(JSON.stringify(applyMutation(base, m))).toBe(JSON.stringify(applyMutation(base, m)))
  })
})
