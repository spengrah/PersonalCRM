import { describe, it, expect } from 'vitest'
import { parseCase, parseMutation } from './schema'

const cleanCase = {
  id: 'CON-041-clean',
  behavior_id: 'CON-041',
  captures: ['contacts/004-edit.json', 'contacts/005-merge.json'],
  expected: [
    { then_index: 0, grader: 'verifier', verdict: 'pass' },
    { then_index: 1, grader: 'verifier', verdict: 'pass' },
  ],
  source: 'clean',
  metadata: { capture_generator_version: 1 },
}

describe('parseCase', () => {
  it('accepts a valid clean case', () => {
    expect(parseCase(cleanCase).id).toBe('CON-041-clean')
  })

  it('accepts a doctored case with a doctor spec', () => {
    const doctored = {
      ...cleanCase,
      id: 'CON-041-doctored',
      source: 'doctored',
      expected: [
        { then_index: 0, grader: 'verifier', verdict: 'pass' },
        { then_index: 1, grader: 'verifier', verdict: 'fail' },
      ],
      doctor: {
        base_case: 'CON-041-clean',
        mutation: { op: 'inject_query', role: 'edit', param: 'action', value: 'edit' },
      },
    }
    expect(parseCase(doctored).doctor?.mutation.op).toBe('inject_query')
  })

  it('rejects a doctored case missing its doctor spec', () => {
    expect(() => parseCase({ ...cleanCase, source: 'doctored' })).toThrow(/requires a doctor spec/)
  })

  it('rejects a clean case that carries a doctor spec', () => {
    expect(() =>
      parseCase({
        ...cleanCase,
        doctor: { base_case: 'x', mutation: { op: 'blank_dialog' } },
      })
    ).toThrow(/must not carry a doctor spec/)
  })

  it('rejects an unknown verdict / missing required field', () => {
    expect(() =>
      parseCase({
        ...cleanCase,
        expected: [{ then_index: 0, grader: 'verifier', verdict: 'maybe' }],
      })
    ).toThrow()
    expect(() => parseCase({ ...cleanCase, captures: [] })).toThrow()
  })
})

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
