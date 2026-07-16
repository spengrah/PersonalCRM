import { describe, it, expect } from 'vitest'
import { parseMutation } from './mutation'

describe('parseMutation', () => {
  it('validates each mutation op', () => {
    expect(parseMutation({ op: 'inject_query', param: 'action', value: 'edit' }).op).toBe(
      'inject_query'
    )
    expect(parseMutation({ op: 'delete_endpoint', endpoint: 'POST /x' }).op).toBe('delete_endpoint')
    expect(
      parseMutation({
        op: 'set_aria_disabled',
        node_role: 'button',
        node_name: 'Previous contact',
        value: false,
      }).op
    ).toBe('set_aria_disabled')
    expect(parseMutation({ op: 'reorder_ids', role: 'detail' })).toMatchObject({
      op: 'reorder_ids',
      mode: 'swap-first-two',
    })
    expect(parseMutation({ op: 'blank_dialog' }).op).toBe('blank_dialog')
    expect(
      parseMutation({ op: 'remove_aria_subtree', node_role: 'link', node_name: 'Add Contact' }).op
    ).toBe('remove_aria_subtree')
    expect(parseMutation({ op: 'set_field', field: 'overdueLoadingSkeletons', value: 0 }).op).toBe(
      'set_field'
    )
    expect(
      parseMutation({
        op: 'set_json_field',
        endpoint: 'GET /api/v1/contacts/:id',
        path: ['data', 'last_outreach_at'],
        value: null,
      }).op
    ).toBe('set_json_field')
  })

  it('rejects an unknown op', () => {
    expect(() => parseMutation({ op: 'nuke' })).toThrow()
  })
})
