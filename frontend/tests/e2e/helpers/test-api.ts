import { APIRequestContext, TestInfo } from '@playwright/test'

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

/**
 * Generates a worker-safe prefix for test data isolation.
 * Format: w{workerIndex}-{timestamp}
 * This ensures parallel test workers don't interfere with each other.
 */
export function getTestPrefix(testInfo: TestInfo): string {
  return `w${testInfo.workerIndex}-${Date.now()}`
}

// ============================================================================
// Types
// ============================================================================

export interface SeedExternalContactInput {
  display_name: string
  emails?: string[]
  phones?: string[]
  organization?: string
  job_title?: string
}

export interface SeedExternalContactsRequest {
  prefix: string
  contacts: SeedExternalContactInput[]
}

export interface SeedExternalContactsResponse {
  created: number
  ids: string[]
}

export interface SeedOverdueContactInput {
  full_name: string
  cadence: 'weekly' | 'biweekly' | 'monthly' | 'quarterly' | 'biannual' | 'annual'
  days_overdue: number
  email?: string
}

export interface SeedOverdueContactsRequest {
  prefix: string
  contacts: SeedOverdueContactInput[]
}

export interface SeedOverdueContactsResponse {
  created: number
  ids: string[]
}

export interface CleanupRequest {
  prefix: string
}

export interface CleanupResponse {
  deleted_contacts: number
  deleted_external_contacts: number
}

export interface TriggerErrorRequest {
  error_type: '500' | 'panic'
  message?: string
}

// ============================================================================
// Test API Client
// ============================================================================

/**
 * TestAPI provides methods to seed and cleanup test data via the backend test endpoints.
 * These endpoints are only available when CRM_ENV=testing.
 */
export class TestAPI {
  private _prefix: string

  constructor(
    private request: APIRequestContext,
    private testInfo: TestInfo
  ) {
    // Generate prefix once at construction time to ensure stability
    this._prefix = `w${testInfo.workerIndex}-${Date.now()}`
  }

  /**
   * Gets the test prefix for this test worker.
   * Use this prefix for all test data to ensure cleanup works correctly.
   * The prefix is generated once at construction time and remains stable.
   */
  get prefix(): string {
    return this._prefix
  }

  /**
   * Seeds external contacts (import candidates) in the database.
   * These will appear on the Imports page.
   */
  async seedExternalContacts(
    contacts: SeedExternalContactInput[]
  ): Promise<SeedExternalContactsResponse> {
    const response = await this.request.post(`${API_BASE_URL}/api/v1/test/seed/external-contacts`, {
      headers: API_HEADERS,
      data: {
        prefix: this.prefix,
        contacts,
      } satisfies SeedExternalContactsRequest,
    })

    if (!response.ok()) {
      const body = await response.text()
      throw new Error(`Failed to seed external contacts: ${response.status()} ${body}`)
    }

    const data = await response.json()
    return data.data as SeedExternalContactsResponse
  }

  /**
   * Seeds contacts with backdated last_contacted timestamps so they appear as overdue.
   * Useful for testing dashboard, overdue lists, and reminder features.
   */
  async seedOverdueContacts(
    contacts: SeedOverdueContactInput[]
  ): Promise<SeedOverdueContactsResponse> {
    const response = await this.request.post(`${API_BASE_URL}/api/v1/test/seed/overdue-contacts`, {
      headers: API_HEADERS,
      data: {
        prefix: this.prefix,
        contacts,
      } satisfies SeedOverdueContactsRequest,
    })

    if (!response.ok()) {
      const body = await response.text()
      throw new Error(`Failed to seed overdue contacts: ${response.status()} ${body}`)
    }

    const data = await response.json()
    return data.data as SeedOverdueContactsResponse
  }

  /**
   * Cleans up all test data created with this test's prefix.
   * Call this in afterEach or afterAll to ensure test isolation.
   */
  async cleanup(): Promise<CleanupResponse> {
    const response = await this.request.post(`${API_BASE_URL}/api/v1/test/cleanup`, {
      headers: API_HEADERS,
      data: {
        prefix: this.prefix,
      } satisfies CleanupRequest,
    })

    if (!response.ok()) {
      const body = await response.text()
      throw new Error(`Failed to cleanup test data: ${response.status()} ${body}`)
    }

    const data = await response.json()
    return data.data as CleanupResponse
  }

  /**
   * Triggers a server error for testing error boundary handling.
   */
  async triggerError(errorType: '500' | 'panic' = '500', message?: string): Promise<void> {
    const response = await this.request.post(`${API_BASE_URL}/api/v1/test/trigger-error`, {
      headers: API_HEADERS,
      data: {
        error_type: errorType,
        message,
      } satisfies TriggerErrorRequest,
    })

    // This endpoint intentionally returns an error
    if (response.status() !== 500) {
      throw new Error(`Expected 500 error but got ${response.status()}`)
    }
  }
}

/**
 * Creates a TestAPI instance for a test.
 * Usage:
 * ```ts
 * test('my test', async ({ request }, testInfo) => {
 *   const testApi = createTestAPI(request, testInfo)
 *   await testApi.seedExternalContacts([{ display_name: 'Test User' }])
 *   // ... run test ...
 *   await testApi.cleanup()
 * })
 * ```
 */
export function createTestAPI(request: APIRequestContext, testInfo: TestInfo): TestAPI {
  return new TestAPI(request, testInfo)
}
