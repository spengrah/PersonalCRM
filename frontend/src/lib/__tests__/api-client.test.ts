/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { apiClient, ApiError } from '../api-client'

describe('ApiClient', () => {
  beforeEach(() => {
    global.fetch = vi.fn()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  describe('successful requests', () => {
    it('makes successful GET request', async () => {
      const mockData = { id: '123', name: 'Test' }
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockData }),
      })

      const result = await apiClient.get('/api/test')
      expect(result).toEqual(mockData)
    })

    it('makes successful POST request', async () => {
      const mockData = { id: '456', name: 'Created' }
      const postData = { name: 'New Item' }
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 201,
        json: async () => ({ success: true, data: mockData }),
      })

      const result = await apiClient.post('/api/test', postData)
      expect(result).toEqual(mockData)
    })

    it('makes successful PUT request', async () => {
      const mockData = { id: '789', name: 'Updated' }
      const putData = { name: 'Updated Item' }
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockData }),
      })

      const result = await apiClient.put('/api/test/789', putData)
      expect(result).toEqual(mockData)
    })

    it('makes successful PATCH request', async () => {
      const mockData = { id: '101', name: 'Patched' }
      const patchData = { name: 'Patched Item' }
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockData }),
      })

      const result = await apiClient.patch('/api/test/101', patchData)
      expect(result).toEqual(mockData)
    })

    it('handles DELETE request with 204 No Content', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 204,
      })

      const result = await apiClient.delete('/api/test/123')
      expect(result).toBeUndefined()
    })

    it('handles GET request with query parameters', async () => {
      const mockData = { items: [] }
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ success: true, data: mockData }),
      })

      await apiClient.get('/api/test', { page: 1, limit: 10 })

      const fetchCall = (global.fetch as any).mock.calls[0]
      expect(fetchCall[0]).toContain('page=1')
      expect(fetchCall[0]).toContain('limit=10')
    })
  })

  describe('error handling', () => {
    it('throws ApiError on HTTP 404', async () => {
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: false,
        status: 404,
        statusText: 'Not Found',
        json: async () => ({
          error: {
            code: 'NOT_FOUND',
            message: 'Resource not found',
          },
        }),
      })

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
      ;(global.fetch as any).mockResolvedValueOnce({
        ok: false,
        status: 503,
        statusText: 'Service Unavailable',
        json: async () => {
          throw new Error('Invalid JSON')
        },
      })

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
      ;(global.fetch as any).mockRejectedValueOnce(new Error('Network failure'))

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
})
