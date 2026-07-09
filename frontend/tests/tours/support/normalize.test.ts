import { describe, it, expect } from 'vitest'
import {
  createUuidMapper,
  mapUuids,
  normalizeUrl,
  parseQuery,
  endpointKey,
  normalizeJson,
  parseAriaSnapshot,
  normalizeAriaTree,
  DEFAULT_ARRAY_CAP,
  DEFAULT_ARIA_CAP,
} from './normalize'
import type { AriaNode } from './types'

const UUID_A = '11111111-1111-4111-8111-111111111111'
const UUID_B = '22222222-2222-4222-8222-222222222222'

describe('UUID mapper', () => {
  it('maps a UUID to a stable run-scoped ordinal', () => {
    const m = createUuidMapper()
    expect(m.map(UUID_A)).toBe('<id:1>')
    expect(m.map(UUID_A)).toBe('<id:1>') // same uuid → same ordinal
    expect(m.map(UUID_B)).toBe('<id:2>')
    expect(m.map(UUID_A)).toBe('<id:1>') // still stable after a second uuid
  })

  it('is case-insensitive on the UUID', () => {
    const m = createUuidMapper()
    expect(m.map(UUID_A.toUpperCase())).toBe('<id:1>')
    expect(m.map(UUID_A.toLowerCase())).toBe('<id:1>')
  })

  it('maps the SAME uuid identically across body, requestUrl, query, and aria', () => {
    const m = createUuidMapper()
    const inBody = normalizeJson({ id: UUID_A }, m, DEFAULT_ARRAY_CAP) as { id: string }
    const inUrl = normalizeUrl(`https://host/api/v1/contacts/${UUID_A}`, m)
    const inQuery = parseQuery(`https://host/api/v1/contacts?source_id=${UUID_A}`, m)
    const inAria = normalizeAriaTree(
      { role: 'link', name: `contact ${UUID_A}` },
      m,
      DEFAULT_ARIA_CAP
    )
    expect(inBody.id).toBe('<id:1>')
    expect(inUrl).toBe('/api/v1/contacts/<id:1>')
    expect(inQuery.source_id).toBe('<id:1>')
    expect(inAria.name).toBe('contact <id:1>')
  })

  it('maps multiple UUIDs inside one string', () => {
    const m = createUuidMapper()
    expect(mapUuids(`${UUID_A} and ${UUID_B}`, m)).toBe('<id:1> and <id:2>')
  })
})

describe('normalizeUrl / parseQuery / endpointKey', () => {
  it('strips the host and preserves path + query with UUIDs mapped', () => {
    const m = createUuidMapper()
    const url = `https://staging.example/api/v1/contacts?sort=cadence&order=desc`
    expect(normalizeUrl(url, m)).toBe('/api/v1/contacts?sort=cadence&order=desc')
  })

  it('preserves and sorts query params, mapping UUID values', () => {
    const m = createUuidMapper()
    const q = parseQuery(`https://h/api/v1/contacts?order=desc&sort=cadence&ids_only=true`, m)
    expect(Object.keys(q)).toEqual(['ids_only', 'order', 'sort'])
    expect(q).toEqual({ ids_only: 'true', order: 'desc', sort: 'cadence' })
  })

  it('templates UUID path segments to :id for the endpoint key', () => {
    expect(
      endpointKey('GET', `https://h/api/v1/contacts/${UUID_A}/merge/preview?source_id=x`)
    ).toBe('GET /api/v1/contacts/:id/merge/preview')
    expect(endpointKey('DELETE', `https://h/api/v1/contacts/${UUID_A}`)).toBe(
      'DELETE /api/v1/contacts/:id'
    )
  })

  it('distinguishes the list, ids_only, and limit=1000 requests by query', () => {
    const m = createUuidMapper()
    expect(parseQuery('https://h/api/v1/contacts?sort=cadence&order=desc', m)).not.toEqual(
      parseQuery('https://h/api/v1/contacts?ids_only=true', m)
    )
    expect(parseQuery('https://h/api/v1/contacts?limit=1000', m).limit).toBe('1000')
  })
})

describe('normalizeJson deny-list vs preserve-list', () => {
  it('sentinels audit-only wall-clock columns but preserves behavior-relevant dates', () => {
    const m = createUuidMapper()
    const out = normalizeJson(
      {
        created_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-02T00:00:00Z',
        contact_by: '2026-03-01T00:00:00Z',
        last_contacted: '2026-02-01T00:00:00Z',
        last_response_at: '2026-02-02T00:00:00Z',
        last_outreach_at: '2026-02-03T00:00:00Z',
        last_interaction_at: '2026-02-04T00:00:00Z',
        birthday: '1990-05-05',
        occurred_at: '2026-02-05T00:00:00Z',
        deleted_at: '2026-02-06T00:00:00Z',
      },
      m,
      DEFAULT_ARRAY_CAP
    ) as Record<string, string>
    expect(out.created_at).toBe('<ts>')
    expect(out.updated_at).toBe('<ts>')
    // Behavior-relevant dates survive verbatim (grader evidence).
    expect(out.contact_by).toBe('2026-03-01T00:00:00Z')
    expect(out.last_contacted).toBe('2026-02-01T00:00:00Z')
    expect(out.last_response_at).toBe('2026-02-02T00:00:00Z')
    expect(out.last_outreach_at).toBe('2026-02-03T00:00:00Z')
    expect(out.last_interaction_at).toBe('2026-02-04T00:00:00Z')
    expect(out.birthday).toBe('1990-05-05')
    expect(out.occurred_at).toBe('2026-02-05T00:00:00Z')
    expect(out.deleted_at).toBe('2026-02-06T00:00:00Z')
  })

  it('redacts transport / token noise but keeps the key structure', () => {
    const m = createUuidMapper()
    const out = normalizeJson(
      { etag: 'W/"abc"', csrf_token: 'zzz', session_id: 'sss', name: 'Contact A' },
      m,
      DEFAULT_ARRAY_CAP
    ) as Record<string, string>
    expect(out.etag).toBe('<redacted>')
    expect(out.csrf_token).toBe('<redacted>')
    expect(out.session_id).toBe('<redacted>')
    expect(out.name).toBe('Contact A')
  })

  it('preserves error envelopes and status evidence (404 body kept)', () => {
    const m = createUuidMapper()
    const out = normalizeJson(
      { success: false, error: { code: 'NOT_FOUND', message: 'Contact not found' } },
      m,
      DEFAULT_ARRAY_CAP
    )
    expect(out).toEqual({
      error: { code: 'NOT_FOUND', message: 'Contact not found' },
      success: false,
    })
  })

  it('stable-sorts object keys recursively', () => {
    const m = createUuidMapper()
    const out = normalizeJson({ b: 1, a: { d: 2, c: 3 } }, m, DEFAULT_ARRAY_CAP)
    expect(JSON.stringify(out)).toBe('{"a":{"c":3,"d":2},"b":1}')
  })

  it('maps UUID values inside bodies and requestBody payloads', () => {
    const m = createUuidMapper()
    const out = normalizeJson(
      { source_contact_id: UUID_A, field_selections: { cadence: 'target' } },
      m,
      DEFAULT_ARRAY_CAP
    )
    expect(out).toEqual({
      field_selections: { cadence: 'target' },
      source_contact_id: '<id:1>',
    })
  })
})

describe('normalizeJson array capping', () => {
  it('caps arrays at the default 50 with a __truncated__ tail', () => {
    const m = createUuidMapper()
    const arr = Array.from({ length: 60 }, (_v, i) => ({ n: i }))
    const out = normalizeJson(arr, m, DEFAULT_ARRAY_CAP) as unknown[]
    expect(out).toHaveLength(DEFAULT_ARRAY_CAP + 1)
    expect(out[DEFAULT_ARRAY_CAP]).toEqual({ __truncated__: 10 })
  })

  it('arrayCap = Infinity preserves the full array (e.g. CON-045 limit=1000)', () => {
    const m = createUuidMapper()
    const arr = Array.from({ length: 300 }, (_v, i) => i)
    const out = normalizeJson(arr, m, Infinity) as unknown[]
    expect(out).toHaveLength(300)
    expect(out.some(v => typeof v === 'object')).toBe(false)
  })
})

describe('parseAriaSnapshot', () => {
  it('parses indentation into nesting', () => {
    const yaml = ['- navigation "Main":', '  - link "Contacts"', '  - link "Birthdays"'].join('\n')
    const root = parseAriaSnapshot(yaml)
    expect(root.children).toHaveLength(1)
    const nav = root.children![0] as AriaNode
    expect(nav.role).toBe('navigation')
    expect(nav.name).toBe('Main')
    expect(nav.children).toHaveLength(2)
    expect((nav.children![0] as AriaNode).role).toBe('link')
    expect((nav.children![1] as AriaNode).name).toBe('Birthdays')
  })

  it('parses state tokens into node fields', () => {
    const yaml = [
      '- button "Previous contact" [disabled]',
      '- button "Next contact"',
      '- checkbox "Agree" [checked]',
      '- checkbox "Partial" [checked=mixed]',
      '- heading "Contacts" [level=2]',
      '- button "Toggle" [pressed] [expanded]',
      '- option "Selected" [selected]',
    ].join('\n')
    const kids = parseAriaSnapshot(yaml).children as AriaNode[]
    expect(kids[0]).toEqual({ role: 'button', name: 'Previous contact', disabled: true })
    expect(kids[1]).toEqual({ role: 'button', name: 'Next contact' })
    expect(kids[2].checked).toBe(true)
    expect(kids[3].checked).toBe('mixed')
    expect(kids[4].level).toBe(2)
    expect(kids[5].pressed).toBe(true)
    expect(kids[5].expanded).toBe(true)
    expect(kids[6].selected).toBe(true)
  })

  it('preserves leaf text nodes verbatim (banner / card copy)', () => {
    const yaml = [
      '- paragraph:',
      '  - text: Contacts merged successfully!', // bare scalar (no quoting needed)
      '- generic: "Value: with colon"', // quoted (a ": " forces quoting)
    ].join('\n')
    const kids = parseAriaSnapshot(yaml).children as AriaNode[]
    const textLeaf = kids[0].children![0] as AriaNode
    expect(textLeaf).toEqual({ role: 'text', text: 'Contacts merged successfully!' })
    // A single inlined text child is preserved as a text leaf too.
    const inlineLeaf = kids[1].children![0] as AriaNode
    expect(inlineLeaf).toEqual({ role: 'text', text: 'Value: with colon' })
  })

  it('drops non-deterministic ref / cursor tokens', () => {
    const yaml = '- button "Delete" [ref=e12] [cursor=pointer]'
    const node = (parseAriaSnapshot(yaml).children as AriaNode[])[0]
    expect(node).toEqual({ role: 'button', name: 'Delete' })
  })

  it('handles a single-quote-wrapped key (name with YAML-special chars)', () => {
    // Playwright wraps the WHOLE createKey (role + name + [tokens]) in single
    // quotes when it needs escaping; '' escapes a literal quote. The ": " in the
    // name is what forces the wrap here.
    const yaml = `- 'button "It''s: here" [disabled]'`
    const node = (parseAriaSnapshot(yaml).children as AriaNode[])[0]
    expect(node.role).toBe('button')
    expect(node.name).toBe("It's: here")
    expect(node.disabled).toBe(true)
  })
})

describe('normalizeAriaTree', () => {
  it('UUID-maps names and text and caps repeated siblings at the default 20', () => {
    const parent: AriaNode = {
      role: 'list',
      children: Array.from({ length: 25 }, (_v, i) => ({
        role: 'listitem' as const,
        name: `item ${i} ${UUID_A}`,
      })),
    }
    const m = createUuidMapper()
    const out = normalizeAriaTree(parent, m, DEFAULT_ARIA_CAP)
    expect(out.children).toHaveLength(DEFAULT_ARIA_CAP + 1)
    expect((out.children![0] as AriaNode).name).toBe('item 0 <id:1>')
    expect(out.children![DEFAULT_ARIA_CAP]).toEqual({ __ariaTruncated__: 5 })
  })

  it('ariaCap = Infinity preserves ALL siblings (e.g. CON-045 birthday cards)', () => {
    const parent: AriaNode = {
      role: 'list',
      children: Array.from({ length: 40 }, () => ({ role: 'listitem' as const })),
    }
    const out = normalizeAriaTree(parent, createUuidMapper(), Infinity)
    expect(out.children).toHaveLength(40)
    expect(out.children!.every(c => 'role' in c)).toBe(true)
  })

  it('carries state fields through normalization', () => {
    const out = normalizeAriaTree(
      { role: 'button', name: 'Previous contact', disabled: true },
      createUuidMapper(),
      DEFAULT_ARIA_CAP
    )
    expect(out).toEqual({ role: 'button', name: 'Previous contact', disabled: true })
  })
})
