import { describe, it, expect } from 'vitest'
import { sortApiResponses } from './sort-responses'
import type { ApiResponseItem, ApiResponses } from './types'

function item(over: Partial<ApiResponseItem>): ApiResponseItem {
  return {
    method: 'GET',
    requestUrl: '/api/v1/contacts',
    query: {},
    status: 200,
    body: null,
    ...over,
  }
}

describe('sortApiResponses', () => {
  it('sorts keys and each endpoint by (method, requestUrl, status), deterministically', () => {
    const shuffled: ApiResponses = {
      'GET /api/v1/contacts': [
        item({ requestUrl: '/api/v1/contacts?ids_only=true', status: 200 }),
        item({ requestUrl: '/api/v1/contacts?sort=cadence', status: 200 }),
        item({ requestUrl: '/api/v1/contacts?sort=cadence', status: 500 }),
      ],
      'DELETE /api/v1/contacts/:id': [item({ method: 'DELETE', status: 204 })],
    }
    const out = sortApiResponses(shuffled)
    // Keys sorted.
    expect(Object.keys(out)).toEqual(['DELETE /api/v1/contacts/:id', 'GET /api/v1/contacts'])
    // Items within the GET group sorted by requestUrl then status.
    expect(out['GET /api/v1/contacts'].map(i => `${i.requestUrl}#${i.status}`)).toEqual([
      '/api/v1/contacts?ids_only=true#200',
      '/api/v1/contacts?sort=cadence#200',
      '/api/v1/contacts?sort=cadence#500',
    ])
  })

  it('is order-independent: a reversed input yields the identical output', () => {
    const a: ApiResponses = {
      'GET /api/v1/contacts': [
        item({ requestUrl: '/api/v1/contacts?a', status: 200 }),
        item({ requestUrl: '/api/v1/contacts?b', status: 200 }),
      ],
    }
    const b: ApiResponses = {
      'GET /api/v1/contacts': [
        item({ requestUrl: '/api/v1/contacts?b', status: 200 }),
        item({ requestUrl: '/api/v1/contacts?a', status: 200 }),
      ],
    }
    expect(JSON.stringify(sortApiResponses(a))).toBe(JSON.stringify(sortApiResponses(b)))
  })
})
