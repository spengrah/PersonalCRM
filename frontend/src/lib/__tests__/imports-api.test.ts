/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { importsApi } from '../imports-api'
import { ApiError } from '../api-client'

describe('importsApi', () => {
  beforeEach(() => {
    global.fetch = vi.fn()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  describe('getCandidates', () => {
    it('fetches import candidates with default pagination', async () => {
      const mockCandidates = [
        {
          id: 'ext-1',
          source: 'gcontacts',
          display_name: 'John Doe',
          emails: ['john@example.com'],
          phones: [],
        },
      ]
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          data: mockCandidates,
          meta: {
            pagination: {
              total: 1,
              page: 1,
              limit: 20,
              pages: 1,
            },
          },
        }),
      })

      const result = await importsApi.getCandidates()

      expect(result.candidates).toEqual(mockCandidates)
      expect(result.total).toBe(1)
      expect(result.page).toBe(1)
      expect(result.limit).toBe(20)
      expect(result.pages).toBe(1)

      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('/api/v1/imports/candidates')
      expect(fetchCall[0]).toContain('page=1')
      expect(fetchCall[0]).toContain('limit=20')
    })

    it('passes custom pagination params', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          data: [],
          meta: { pagination: { total: 0, page: 2, limit: 10, pages: 0 } },
        }),
      })

      await importsApi.getCandidates({ page: 2, limit: 10 })

      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('page=2')
      expect(fetchCall[0]).toContain('limit=10')
    })

    it('passes source filter when provided', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          data: [],
          meta: { pagination: { total: 0, page: 1, limit: 20, pages: 0 } },
        }),
      })

      await importsApi.getCandidates({ source: 'gcontacts' })

      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('source=gcontacts')
    })

    it('handles empty response gracefully', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({}),
      })

      const result = await importsApi.getCandidates()

      expect(result.candidates).toEqual([])
      expect(result.total).toBe(0)
      expect(result.page).toBe(1)
      expect(result.limit).toBe(20)
      expect(result.pages).toBe(0)
    })

    it('throws ApiError on HTTP 404', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: false,
        status: 404,
        statusText: 'Not Found',
        json: async () => ({
          error: {
            code: 'NOT_FOUND',
            message: 'Endpoint not found',
          },
        }),
      })

      try {
        await importsApi.getCandidates()
        expect.fail('Should have thrown an error')
      } catch (error) {
        expect(error).toBeInstanceOf(ApiError)
        expect((error as ApiError).status).toBe(404)
        expect((error as ApiError).code).toBe('NOT_FOUND')
      }
    })

    it('throws ApiError on HTTP 500', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: false,
        status: 500,
        statusText: 'Internal Server Error',
        json: async () => ({
          error: {
            code: 'INTERNAL_ERROR',
            message: 'Something went wrong',
          },
        }),
      })

      await expect(importsApi.getCandidates()).rejects.toThrow(ApiError)
    })

    it('handles error response without JSON body', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: false,
        status: 503,
        statusText: 'Service Unavailable',
        json: async () => {
          throw new Error('Invalid JSON')
        },
      })

      try {
        await importsApi.getCandidates()
        expect.fail('Should have thrown an error')
      } catch (error) {
        expect(error).toBeInstanceOf(ApiError)
        expect((error as ApiError).status).toBe(503)
        expect((error as ApiError).message).toBe('HTTP 503: Service Unavailable')
      }
    })
  })

  describe('getCandidate', () => {
    it('fetches single import candidate by id', async () => {
      const mockCandidate = {
        id: 'ext-123',
        source: 'gcontacts',
        display_name: 'Jane Smith',
        first_name: 'Jane',
        last_name: 'Smith',
        emails: ['jane@example.com'],
        phones: ['+1234567890'],
        organization: 'Acme Corp',
        job_title: 'Engineer',
      }
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockCandidate }),
      })

      const result = await importsApi.getCandidate('ext-123')

      expect(result).toEqual(mockCandidate)
      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('/api/v1/imports/ext-123')
    })
  })

  describe('importCandidate', () => {
    it('imports candidate as new CRM contact', async () => {
      const mockContact = {
        id: 'crm-456',
        full_name: 'John Doe',
        contact_frequency_days: 30,
      }
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 201,
        json: async () => ({ success: true, data: mockContact }),
      })

      const result = await importsApi.importCandidate('ext-123')

      expect(result).toEqual(mockContact)
      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('/api/v1/imports/ext-123/import')
      expect(fetchCall[1].method).toBe('POST')
    })
  })

  describe('linkCandidate', () => {
    it('links candidate to existing CRM contact', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true }),
      })

      await importsApi.linkCandidate('ext-123', { crm_contact_id: 'crm-456' })

      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('/api/v1/imports/ext-123/link')
      expect(fetchCall[1].method).toBe('POST')
      expect(JSON.parse(fetchCall[1].body)).toEqual({ crm_contact_id: 'crm-456' })
    })

    it('links candidate with method selection and conflict resolutions', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true }),
      })

      const request = {
        crm_contact_id: 'crm-456',
        selected_methods: [{ original_value: 'john@work.com', type: 'email_work' }],
        conflict_resolutions: {
          'john@work.com': 'use_external' as const,
        },
      }

      await importsApi.linkCandidate('ext-123', request)

      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('/api/v1/imports/ext-123/link')
      expect(fetchCall[1].method).toBe('POST')
      expect(JSON.parse(fetchCall[1].body)).toEqual(request)
    })
  })

  describe('ignoreCandidate', () => {
    it('marks candidate as ignored', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true }),
      })

      await importsApi.ignoreCandidate('ext-123')

      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('/api/v1/imports/ext-123/ignore')
      expect(fetchCall[1].method).toBe('POST')
    })
  })

  describe('triggerSync', () => {
    it('triggers sync with default source', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true }),
      })

      await importsApi.triggerSync()

      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('/api/v1/sync/gcontacts/trigger')
      expect(fetchCall[1].method).toBe('POST')
    })

    it('triggers sync with custom source', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true }),
      })

      await importsApi.triggerSync('icloud')

      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('/api/v1/sync/icloud/trigger')
    })
  })
})
