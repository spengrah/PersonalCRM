import { describe, expect, it } from 'vitest'

import {
  DEFAULT_SORT_FIELD,
  DEFAULT_SORT_ORDER,
  buildContactDetailUrl,
  buildContactListUrl,
  defaultOrderFor,
  listContextSearchParams,
  parseListContext,
  type ContactListContext,
} from '../contact-list-params'

describe('contact-list-params', () => {
  describe('parseListContext', () => {
    it('falls back to defaults for an empty URL', () => {
      const ctx = parseListContext(new URLSearchParams())
      expect(ctx).toEqual({ sort: DEFAULT_SORT_FIELD, order: DEFAULT_SORT_ORDER })
    })

    it('reads all five params', () => {
      const ctx = parseListContext(
        new URLSearchParams(
          'sort=name&order=asc&search=jane&cadence_filter=has_cadence&followup_filter=no_followup'
        )
      )
      expect(ctx).toEqual({
        sort: 'name',
        order: 'asc',
        search: 'jane',
        cadence_filter: 'has_cadence',
        followup_filter: 'no_followup',
      })
    })

    it('rejects invalid values and falls back to defaults', () => {
      const ctx = parseListContext(
        new URLSearchParams('sort=DROP+TABLE&order=sideways&cadence_filter=bogus&followup_filter=x')
      )
      expect(ctx).toEqual({ sort: DEFAULT_SORT_FIELD, order: DEFAULT_SORT_ORDER })
    })

    it('treats empty strings as absent', () => {
      const ctx = parseListContext(new URLSearchParams('sort=&order=&search=&cadence_filter='))
      expect(ctx).toEqual({ sort: DEFAULT_SORT_FIELD, order: DEFAULT_SORT_ORDER })
    })
  })

  describe('round-trip symmetry', () => {
    it('parse(build(ctx)) returns the same context, filters included', () => {
      const ctx: ContactListContext = {
        sort: 'birthday',
        order: 'asc',
        search: 'smith',
        cadence_filter: 'no_cadence',
        followup_filter: 'has_followup',
      }
      const roundTripped = parseListContext(listContextSearchParams(ctx))
      expect(roundTripped).toEqual(ctx)
    })

    it('carries followup_filter through detail URLs (regression: buildContactUrl dropped it)', () => {
      const ctx: ContactListContext = {
        sort: 'cadence',
        order: 'desc',
        followup_filter: 'has_followup',
      }
      const url = buildContactDetailUrl(ctx, 'abc-123')
      expect(url).toContain('followup_filter=has_followup')
      const parsed = parseListContext(new URL(url, 'http://x').searchParams)
      expect(parsed.followup_filter).toBe('has_followup')
    })
  })

  describe('URL builders', () => {
    const ctx: ContactListContext = { sort: 'name', order: 'asc', search: 'q' }

    it('always writes sort and order, even at defaults', () => {
      const url = buildContactListUrl({ sort: DEFAULT_SORT_FIELD, order: DEFAULT_SORT_ORDER })
      expect(url).toBe('/contacts?sort=cadence&order=desc')
    })

    it('builds detail URLs with the action param last', () => {
      expect(buildContactDetailUrl(ctx, 'id-1', 'edit')).toBe(
        '/contacts/id-1?sort=name&order=asc&search=q&action=edit'
      )
    })

    it('omits absent search and filters', () => {
      expect(buildContactListUrl({ sort: 'name', order: 'asc' })).toBe(
        '/contacts?sort=name&order=asc'
      )
    })
  })

  describe('defaultOrderFor', () => {
    it('sorts cadence and last_response_at descending by default', () => {
      expect(defaultOrderFor('cadence')).toBe('desc')
      expect(defaultOrderFor('last_response_at')).toBe('desc')
    })

    it('sorts the remaining fields ascending by default', () => {
      for (const field of [
        'name',
        'location',
        'birthday',
        'last_contacted',
        'contact_by',
      ] as const) {
        expect(defaultOrderFor(field)).toBe('asc')
      }
    })
  })
})
