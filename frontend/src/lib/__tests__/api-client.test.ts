import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { apiClient, ApiError } from '../api-client'

describe('ApiClient', () => {
  const originalWindow = global.window

  beforeEach(() => {
    global.fetch = vi.fn()
  })

  afterEach(() => {
    vi.restoreAllMocks()
    global.window = originalWindow
  })

  describe('SSR context', () => {
    it('handles GET request with params when window is undefined', async () => {
      // Simulate SSR context
      Object.assign(global, { window: undefined })

      const mockData = [{ id: '1' }]
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockData }),
      } as unknown as Response)

      // This should not throw even without a valid base URL
      await apiClient.get('/api/test', { page: 1, limit: 10 })

      const fetchCall = vi.mocked(global.fetch).mock.calls[0]
      // In SSR context with empty base, the URL will just be the endpoint with params
      expect(fetchCall[0]).toContain('/api/test')
      expect(fetchCall[0]).toContain('page=1')
      expect(fetchCall[0]).toContain('limit=10')
    })

    it('handles getWithMeta request when window is undefined', async () => {
      // Simulate SSR context
      Object.assign(global, { window: undefined })

      const mockData = [{ id: '1' }]
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          data: mockData,
          meta: { pagination: { total: 1, page: 1, limit: 10, pages: 1 } },
        }),
      } as unknown as Response)

      const result = await apiClient.getWithMeta('/api/test', { page: 1 })

      expect(result.data).toEqual(mockData)
      expect(result.meta?.pagination?.total).toBe(1)
    })
  })

  describe('successful requests', () => {
    it('makes successful GET request', async () => {
      const mockData = { id: '123', name: 'Test' }
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockData }),
      } as unknown as Response)

      const result = await apiClient.get('/api/test')
      expect(result).toEqual(mockData)
    })

    it('makes successful POST request', async () => {
      const mockData = { id: '456', name: 'Created' }
      const postData = { name: 'New Item' }
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 201,
        json: async () => ({ success: true, data: mockData }),
      } as unknown as Response)

      const result = await apiClient.post('/api/test', postData)
      expect(result).toEqual(mockData)
    })

    it('makes successful PUT request', async () => {
      const mockData = { id: '789', name: 'Updated' }
      const putData = { name: 'Updated Item' }
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockData }),
      } as unknown as Response)

      const result = await apiClient.put('/api/test/789', putData)
      expect(result).toEqual(mockData)
    })

    it('makes successful PATCH request', async () => {
      const mockData = { id: '101', name: 'Patched' }
      const patchData = { name: 'Patched Item' }
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockData }),
      } as unknown as Response)

      const result = await apiClient.patch('/api/test/101', patchData)
      expect(result).toEqual(mockData)
    })

    it('handles DELETE request with 204 No Content', async () => {
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 204,
      } as unknown as Response)

      const result = await apiClient.delete('/api/test/123')
      expect(result).toBeUndefined()
    })

    it('handles GET request with query parameters', async () => {
      const mockData = { items: [] }
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockData }),
      } as unknown as Response)

      await apiClient.get('/api/test', { page: 1, limit: 10 })

      const fetchCall = vi.mocked(global.fetch).mock.calls[0]
      expect(fetchCall[0]).toContain('page=1')
      expect(fetchCall[0]).toContain('limit=10')
    })
  })

  describe('error handling', () => {
    it('throws ApiError on HTTP 404', async () => {
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: false,
        status: 404,
        statusText: 'Not Found',
        json: async () => ({
          error: {
            code: 'NOT_FOUND',
            message: 'Resource not found',
          },
        }),
      } as unknown as Response)

      try {
        await apiClient.get('/api/test')
        expect.fail('Should have thrown an error')
      } catch (error) {
        expect(error).toBeInstanceOf(ApiError)
        expect((error as ApiError).status).toBe(404)
        expect((error as ApiError).code).toBe('NOT_FOUND')
        expect((error as ApiError).message).toBe('Resource not found')
      }
    })

    it('throws ApiError on HTTP 500', async () => {
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: false,
        status: 500,
        statusText: 'Internal Server Error',
        json: async () => ({
          error: {
            code: 'INTERNAL_ERROR',
            message: 'Something went wrong',
          },
        }),
      } as unknown as Response)

      try {
        await apiClient.get('/api/test')
        expect.fail('Should have thrown an error')
      } catch (error) {
        expect(error).toBeInstanceOf(ApiError)
        expect((error as ApiError).status).toBe(500)
        expect((error as ApiError).code).toBe('INTERNAL_ERROR')
      }
    })

    it('handles error response without JSON body', async () => {
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: false,
        status: 503,
        statusText: 'Service Unavailable',
        json: async () => {
          throw new Error('Invalid JSON')
        },
      } as unknown as Response)

      try {
        await apiClient.get('/api/test')
        expect.fail('Should have thrown an error')
      } catch (error) {
        expect(error).toBeInstanceOf(ApiError)
        expect((error as ApiError).status).toBe(503)
        expect((error as ApiError).message).toBe('HTTP 503: Service Unavailable')
      }
    })

    it('throws ApiError on network error', async () => {
      vi.mocked(global.fetch).mockRejectedValueOnce(new Error('Network failure'))

      try {
        await apiClient.get('/api/test')
        expect.fail('Should have thrown an error')
      } catch (error) {
        expect(error).toBeInstanceOf(ApiError)
        expect((error as ApiError).status).toBe(0)
        expect((error as ApiError).code).toBe('NETWORK_ERROR')
        expect((error as ApiError).message).toBe('Network failure')
      }
    })
  })

  describe('ApiError class', () => {
    it('creates error with correct properties', () => {
      const error = new ApiError('Test error', 404, 'NOT_FOUND')

      expect(error).toBeInstanceOf(Error)
      expect(error.name).toBe('ApiError')
      expect(error.message).toBe('Test error')
      expect(error.status).toBe(404)
      expect(error.code).toBe('NOT_FOUND')
    })

    it('works with instanceof Error', () => {
      const error = new ApiError('Test error', 500)
      expect(error instanceof Error).toBe(true)
    })
  })

  describe('getWithMeta', () => {
    it('returns data and pagination metadata', async () => {
      const mockData = [{ id: '1', name: 'Test' }]
      const mockPagination = { total: 50, page: 1, limit: 20, pages: 3 }
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          data: mockData,
          meta: { pagination: mockPagination },
        }),
      } as unknown as Response)

      const result = await apiClient.getWithMeta('/api/test')

      expect(result.data).toEqual(mockData)
      expect(result.meta?.pagination).toEqual(mockPagination)
    })

    it('handles response without meta field', async () => {
      const mockData = [{ id: '1', name: 'Test' }]
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          data: mockData,
        }),
      } as unknown as Response)

      const result = await apiClient.getWithMeta('/api/test')

      expect(result.data).toEqual(mockData)
      expect(result.meta).toBeUndefined()
    })

    it('handles response with empty pagination', async () => {
      const mockData: unknown[] = []
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          data: mockData,
          meta: { pagination: { total: 0, page: 1, limit: 20, pages: 0 } },
        }),
      } as unknown as Response)

      const result = await apiClient.getWithMeta('/api/test')

      expect(result.data).toEqual([])
      expect(result.meta?.pagination?.total).toBe(0)
      expect(result.meta?.pagination?.pages).toBe(0)
    })

    it('passes query parameters correctly', async () => {
      const mockData = [{ id: '1' }]
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          success: true,
          data: mockData,
          meta: { pagination: { total: 1, page: 2, limit: 10, pages: 1 } },
        }),
      } as unknown as Response)

      await apiClient.getWithMeta('/api/test', { page: 2, limit: 10, search: 'foo' })

      const fetchCall = vi.mocked(global.fetch).mock.calls[0]
      expect(fetchCall[0]).toContain('page=2')
      expect(fetchCall[0]).toContain('limit=10')
      expect(fetchCall[0]).toContain('search=foo')
    })

    it('throws ApiError on HTTP error', async () => {
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: false,
        status: 404,
        statusText: 'Not Found',
        json: async () => ({
          error: {
            code: 'NOT_FOUND',
            message: 'Resource not found',
          },
        }),
      } as unknown as Response)

      try {
        await apiClient.getWithMeta('/api/test')
        expect.fail('Should have thrown an error')
      } catch (error) {
        expect(error).toBeInstanceOf(ApiError)
        expect((error as ApiError).status).toBe(404)
        expect((error as ApiError).code).toBe('NOT_FOUND')
      }
    })

    it('handles 204 No Content response', async () => {
      vi.mocked(global.fetch).mockResolvedValueOnce({
        ok: true,
        status: 204,
      } as unknown as Response)

      const result = await apiClient.getWithMeta('/api/test')

      expect(result.data).toBeUndefined()
      expect(result.meta).toBeUndefined()
    })
  })
})
