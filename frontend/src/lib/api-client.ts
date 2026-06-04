const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || ''

class ApiError extends Error {
  status: number
  code?: string

  constructor(message: string, status: number, code?: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

interface ApiResponse<T = unknown> {
  success: boolean
  data?: T
  error?: {
    code: string
    message: string
    details?: Record<string, unknown>
  }
  meta?: {
    pagination?: {
      total: number
      page: number
      limit: number
      pages: number
    }
    hidden_unresolved_telegram_count?: number
  }
}

interface ApiResponseWithMeta<T> {
  data: T
  meta?: ApiResponse['meta']
}

interface ApiResponseWithStatus<T> {
  data: T
  status: number
}

class ApiClient {
  private baseUrl: string

  constructor(baseUrl: string = API_BASE_URL) {
    this.baseUrl = baseUrl
  }

  private getBase(): string {
    return this.baseUrl || (typeof window !== 'undefined' ? window.location.origin : '')
  }

  private buildUrl(endpoint: string, params?: Record<string, unknown>): string {
    const base = this.getBase()
    // Handle SSR context where base may be empty
    if (!base) {
      const searchParams = new URLSearchParams()
      if (params) {
        Object.entries(params).forEach(([key, value]) => {
          if (value !== undefined && value !== null) {
            searchParams.append(key, String(value))
          }
        })
      }
      const search = searchParams.toString()
      return search ? `${endpoint}?${search}` : endpoint
    }

    const url = new URL(endpoint, base)
    if (params) {
      Object.entries(params).forEach(([key, value]) => {
        if (value !== undefined && value !== null) {
          url.searchParams.append(key, String(value))
        }
      })
    }
    return url.pathname + url.search
  }

  private async requestWithMeta<T>(
    endpoint: string,
    options: RequestInit = {}
  ): Promise<ApiResponseWithMeta<T>> {
    const url = `${this.getBase()}${endpoint}`

    // Add timeout to prevent hanging requests
    const controller = new AbortController()
    const timeoutId = setTimeout(() => controller.abort(), 10000) // 10 second timeout

    const config: RequestInit = {
      headers: {
        'Content-Type': 'application/json',
        'X-API-Key': process.env.NEXT_PUBLIC_API_KEY || '',
        ...options.headers,
      },
      signal: controller.signal,
      ...options,
    }

    try {
      const response = await fetch(url, config)
      clearTimeout(timeoutId) // Clear timeout on successful response

      if (!response.ok) {
        let errorMessage = `HTTP ${response.status}: ${response.statusText}`
        let errorCode = 'UNKNOWN_ERROR'

        try {
          const errorData: ApiResponse = await response.json()
          if (errorData.error) {
            errorMessage = errorData.error.message
            errorCode = errorData.error.code
          }
        } catch {
          // If we can't parse the error response, use the default message
        }

        throw new ApiError(errorMessage, response.status, errorCode)
      }

      // Handle 204 No Content responses (e.g., DELETE operations)
      if (response.status === 204) {
        return { data: undefined as T, meta: undefined }
      }

      const data: ApiResponse<T> = await response.json()

      if (!data.success && data.error) {
        throw new ApiError(data.error.message, 400, data.error.code)
      }

      return { data: data.data as T, meta: data.meta }
    } catch (error) {
      clearTimeout(timeoutId) // Clear timeout on error

      if (error instanceof ApiError) {
        throw error
      }

      // Handle timeout errors specifically
      if (error instanceof Error && error.name === 'AbortError') {
        throw new ApiError('Request timed out', 408, 'TIMEOUT_ERROR')
      }

      // Network or other errors
      throw new ApiError(
        error instanceof Error ? error.message : 'An unexpected error occurred',
        0,
        'NETWORK_ERROR'
      )
    }
  }

  private async request<T>(endpoint: string, options: RequestInit = {}): Promise<T> {
    const response = await this.requestWithMeta<T>(endpoint, options)
    return response.data
  }

  // GET request with metadata (for paginated responses)
  async getWithMeta<T>(
    endpoint: string,
    params?: Record<string, unknown>
  ): Promise<ApiResponseWithMeta<T>> {
    return this.requestWithMeta<T>(this.buildUrl(endpoint, params))
  }

  // GET request
  async get<T>(endpoint: string, params?: Record<string, unknown>): Promise<T> {
    return this.request<T>(this.buildUrl(endpoint, params))
  }

  // POST request
  async post<T>(endpoint: string, data?: unknown): Promise<T> {
    return this.request<T>(endpoint, {
      method: 'POST',
      body: data ? JSON.stringify(data) : undefined,
    })
  }

  // PUT request
  async put<T>(endpoint: string, data?: unknown): Promise<T> {
    return this.request<T>(endpoint, {
      method: 'PUT',
      body: data ? JSON.stringify(data) : undefined,
    })
  }

  // PATCH request
  async patch<T>(endpoint: string, data?: unknown): Promise<T> {
    return this.request<T>(endpoint, {
      method: 'PATCH',
      body: data ? JSON.stringify(data) : undefined,
    })
  }

  // DELETE request
  async delete<T>(endpoint: string): Promise<T> {
    return this.request<T>(endpoint, {
      method: 'DELETE',
    })
  }

  // GET request with status (for endpoints that may return 204)
  async getWithStatus<T>(
    endpoint: string,
    params?: Record<string, unknown>
  ): Promise<ApiResponseWithStatus<T>> {
    return this.requestWithStatus<T>(this.buildUrl(endpoint, params))
  }

  // PUT request with status (for endpoints that may return 204)
  async putWithStatus<T>(endpoint: string, data?: unknown): Promise<ApiResponseWithStatus<T>> {
    return this.requestWithStatus<T>(endpoint, {
      method: 'PUT',
      body: data ? JSON.stringify(data) : undefined,
    })
  }

  // Request that returns status along with data (for handling 204 responses)
  private async requestWithStatus<T>(
    endpoint: string,
    options: RequestInit = {}
  ): Promise<ApiResponseWithStatus<T>> {
    const url = `${this.getBase()}${endpoint}`

    const controller = new AbortController()
    const timeoutId = setTimeout(() => controller.abort(), 10000)

    const config: RequestInit = {
      headers: {
        'Content-Type': 'application/json',
        'X-API-Key': process.env.NEXT_PUBLIC_API_KEY || '',
        ...options.headers,
      },
      signal: controller.signal,
      ...options,
    }

    try {
      const response = await fetch(url, config)
      clearTimeout(timeoutId)

      if (!response.ok) {
        let errorMessage = `HTTP ${response.status}: ${response.statusText}`
        let errorCode = 'UNKNOWN_ERROR'

        try {
          const errorData: ApiResponse = await response.json()
          if (errorData.error) {
            errorMessage = errorData.error.message
            errorCode = errorData.error.code
          }
        } catch {
          // If we can't parse the error response, use the default message
        }

        throw new ApiError(errorMessage, response.status, errorCode)
      }

      // Handle 204 No Content responses
      if (response.status === 204) {
        return { data: undefined as T, status: response.status }
      }

      const data: ApiResponse<T> = await response.json()

      if (!data.success && data.error) {
        throw new ApiError(data.error.message, 400, data.error.code)
      }

      return { data: data.data as T, status: response.status }
    } catch (error) {
      clearTimeout(timeoutId)

      if (error instanceof ApiError) {
        throw error
      }

      if (error instanceof Error && error.name === 'AbortError') {
        throw new ApiError('Request timed out', 408, 'TIMEOUT_ERROR')
      }

      throw new ApiError(
        error instanceof Error ? error.message : 'An unexpected error occurred',
        0,
        'NETWORK_ERROR'
      )
    }
  }
}

export const apiClient = new ApiClient()
export { ApiError }
export type { ApiResponse, ApiResponseWithMeta, ApiResponseWithStatus }
