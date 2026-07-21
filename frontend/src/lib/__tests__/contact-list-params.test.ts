import { describe, expect, it } from 'vitest'

import {
  CONTACTS_PAGE_SIZE,
  DEFAULT_SORT_FIELD,
  DEFAULT_SORT_ORDER,
  buildContactDetailUrl,
  buildContactListUrl,
  defaultOrderFor,
  listContextSearchParams,
  pageFromIndex,
  parseListContext,
  parseListPage,
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

  describe('buildContactListUrl page arg', () => {
    const ctx: ContactListContext = { sort: 'name', order: 'asc', search: 'q' }

    it('appends &page=N after the context params when page > 1', () => {
      expect(buildContactListUrl(ctx, 2)).toBe('/contacts?sort=name&order=asc&search=q&page=2')
    })

    it('omits page for page 1 (the canonical bare URL)', () => {
      expect(buildContactListUrl(ctx, 1)).toBe('/contacts?sort=name&order=asc&search=q')
    })

    it('omits page for undefined / 0 / negative / non-integer values', () => {
      const bare = '/contacts?sort=name&order=asc&search=q'
      expect(buildContactListUrl(ctx)).toBe(bare)
      expect(buildContactListUrl(ctx, 0)).toBe(bare)
      expect(buildContactListUrl(ctx, -3)).toBe(bare)
      expect(buildContactListUrl(ctx, 1.5)).toBe(bare)
      expect(buildContactListUrl(ctx, NaN)).toBe(bare)
    })

    it('does not leak page back into the parsed context', () => {
      const parsed = parseListContext(new URL(buildContactListUrl(ctx, 3), 'http://x').searchParams)
      expect(parsed).toEqual(ctx)
      expect('page' in parsed).toBe(false)
    })
  })

  describe('parseListPage', () => {
    it('reads a valid 1-based page from the URL', () => {
      expect(parseListPage(new URLSearchParams('page=3'))).toBe(3)
    })

    it('falls back to page 1 for missing / malformed / zero / negative / fractional', () => {
      expect(parseListPage(new URLSearchParams(''))).toBe(1)
      expect(parseListPage(new URLSearchParams('page=abc'))).toBe(1)
      expect(parseListPage(new URLSearchParams('page=0'))).toBe(1)
      expect(parseListPage(new URLSearchParams('page=-1'))).toBe(1)
      expect(parseListPage(new URLSearchParams('page=1.5'))).toBe(1)
    })
  })

  describe('pageFromIndex', () => {
    it('maps a global index to its 1-based page, and < 0 to undefined', () => {
      expect(pageFromIndex(-1)).toBeUndefined()
      expect(pageFromIndex(0)).toBe(1)
      expect(pageFromIndex(19)).toBe(1)
      expect(pageFromIndex(20)).toBe(2)
      expect(pageFromIndex(39)).toBe(2)
      expect(pageFromIndex(40)).toBe(3)
    })
  })

  describe('CONTACTS_PAGE_SIZE', () => {
    it('is 20 — the list limit and the pageFromIndex divisor must not drift', () => {
      expect(CONTACTS_PAGE_SIZE).toBe(20)
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
