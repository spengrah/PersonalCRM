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

export interface SeedContactMethodInput {
  type: 'email' | 'phone' | 'telegram' | 'discord' | 'twitter' | 'signal' | 'gchat' | 'whatsapp'
  value: string
  is_primary?: boolean
}

export interface SeedContactInput {
  full_name: string
  location?: string
  // Note: notes are no longer stored on the contact table.
  // If tests need to seed notes, call seedContactNote() separately after seeding contacts.
  cadence?: 'weekly' | 'biweekly' | 'monthly' | 'quarterly' | 'biannual' | 'annual'
  methods?: SeedContactMethodInput[]
  last_contacted_days_ago?: number
}

export interface SeedContactsRequest {
  prefix: string
  contacts: SeedContactInput[]
}

export interface SeedContactsResponse {
  created: number
  ids: string[]
}

export interface SeedExternalContactInput {
  display_name?: string
  first_name?: string
  last_name?: string
  source?:
    | 'test'
    | 'telegram'
    | 'gcontacts'
    | 'gcal_attendee'
    | 'icloud_contacts'
    | 'anarlog_humans'
    | 'anarlog_title'
    | 'gmail_correspondence'
  emails?: string[]
  phones?: string[]
  organization?: string
  job_title?: string
  metadata?: Record<string, unknown>
}

export interface SeedExternalContactsRequest {
  prefix: string
  contacts: SeedExternalContactInput[]
}

export interface SeedExternalContactsResponse {
  created: number
  ids: string[]
}

export interface SeedMethodSuggestionMethodInput {
  type: 'email' | 'phone'
  value: string
}

export interface SeedMethodSuggestionsRequest {
  prefix: string
  contact_name: string
  source?: 'gcontacts' | 'icloud_contacts'
  pending: SeedMethodSuggestionMethodInput[]
  dismissed?: SeedMethodSuggestionMethodInput[]
}

export interface SeedMethodSuggestionsResponse {
  external_contact_id: string
  contact_id: string
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

export interface SeedCalendarEventInput {
  title: string
  location?: string
  html_link?: string
  is_past?: boolean
  days_ago?: number
  days_ahead?: number
  attendee_emails?: string[]
  // Seed the event with attendees but no matched_contact_ids — the caller
  // intends to drive the rematch flow from the UI.
  unmatched?: boolean
}

export interface SeedCalendarEventsRequest {
  prefix: string
  contact_id: string
  events: SeedCalendarEventInput[]
}

export interface SeedCalendarEventsResponse {
  created: number
  ids: string[]
}

export interface SeedMacHostRequest {
  hostname?: string
  daemon_version?: string
  protocol_version?: number
  permissions?: Record<string, unknown>
  source_health?: Record<string, unknown>
}

export interface SeedMacHostResponse {
  host_id: string
}

export interface SeedMeetingNoteInput {
  anarlog_session_id: string
  title?: string
  summary?: string
}

export interface SeedMeetingNotesRequest {
  host_id: string
  notes: SeedMeetingNoteInput[]
}

export interface SeedMeetingNotesResponse {
  created: number
  ids: string[]
}

export interface CleanupRequest {
  prefix: string
  // When set, also hard-deletes the host's seeded meeting_note rows.
  host_id?: string
}

/**
 * Declared seeding: a test names a SPEC BEHAVIOR and the server executes that
 * behavior's declared fixture, returning a manifest of what it created.
 * Assertions read names and ids from the manifest — never re-derived strings —
 * because the generator owns them.
 */
export interface SeededEntity {
  kind: string
  id: string
  name: string
}

export interface SeedBehaviorResult {
  /** The EFFECTIVE namespace, which may differ from the one requested. */
  namespace: string
  /** The generator anchor the world was built against. */
  anchor: string
  /** Declaration handle → created row. */
  entities: Record<string, SeededEntity>
}

export type NamespaceCleanupStatus = 'cleaned' | 'busy' | 'pending' | 'error'

export interface NamespaceCleanupOutcome {
  status: NamespaceCleanupStatus
  deleted?: Record<string, number>
  descendants?: string[]
  error?: string
}

export interface CleanupNamespacesResponse {
  expansions: Record<string, string[]>
  results: Record<string, NamespaceCleanupOutcome>
}

export interface CleanupResponse {
  deleted_contacts: number
  deleted_external_contacts: number
  deleted_calendar_events: number
}

export interface TriggerErrorRequest {
  error_type: '500' | 'panic'
  message?: string
}

// ============================================================================
// Test API Client
// ============================================================================

// How long declared-namespace cleanup keeps polling a retriable outcome.
//
// A declared seed runs DETACHED on the server: a seedBehavior call that timed
// out or lost its response does not stop it, and it holds the namespace
// reservation — which cleanup reports as `busy` — for its whole residence. The
// server's own constants bound that residence: a 90s run budget, at most one
// in-flight 30s settle timer, a 30s Gate-B wait in the failure-path teardown
// and a 45s teardown budget (backend/internal/synthetic/declare/run.go,
// backend/internal/synthetic/replay). A poll that gives up before that window
// closes leaves the run's rows in the shared E2E database, which is exactly
// what the pre-registered cleanup handle exists to prevent.
const DECLARED_CLEANUP_POLL_BUDGET_MS = 200_000
const DECLARED_CLEANUP_POLL_INTERVAL_MS = 2_000

// The server's per-request namespace cap (maxCleanupNamespaces in
// backend/internal/synthetic/declare/cleanup.go). It exists so a repeated
// cleanup cannot multiply database work, and it is enforced as a 400 BEFORE
// anything is deleted — so a single oversized request would leave EVERY
// declared world of that test behind, not just the ones past the limit. A test
// reaches it sooner than the number suggests: each seed can record two tokens
// (the requested one and the effective one a re-salt produced), so 16 seeds are
// enough. Cleanup therefore sends in batches of this size.
const DECLARED_CLEANUP_MAX_PER_REQUEST = 32

/**
 * TestAPI provides methods to seed and cleanup test data via the backend test endpoints.
 * These endpoints are only available when CRM_ENV=testing.
 */
export class TestAPI {
  private _prefix: string
  // The most recently seeded mac_host, so cleanup() can scope meeting_note
  // deletion to this test's host without the caller threading the id.
  private _seededHostId: string | null = null
  // Every namespace this test has asked the server to seed. Entries are pushed
  // BEFORE the request goes out, so a lost response still leaves a cleanup
  // handle behind — see seedBehavior.
  private _declaredNamespaces: string[] = []
  private _seedBehaviorCalls = 0
  // Whether the declared-cleanup poll has already bought itself room in the
  // test's timeout slot — see grantDeclaredCleanupBudget.
  private _cleanupBudgetGranted = false

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
   * Seeds contacts in the database with full field support.
   * Supports location, notes, cadence, methods, and backdated last_contacted.
   * Contact names are automatically prefixed for cleanup.
   */
  async seedContacts(contacts: SeedContactInput[]): Promise<SeedContactsResponse> {
    const response = await this.request.post(`${API_BASE_URL}/api/v1/test/seed/contacts`, {
      headers: API_HEADERS,
      data: {
        prefix: this.prefix,
        contacts,
      } satisfies SeedContactsRequest,
    })

    if (!response.ok()) {
      const body = await response.text()
      throw new Error(`Failed to seed contacts: ${response.status()} ${body}`)
    }

    const data = await response.json()
    return data.data as SeedContactsResponse
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
   * Seeds a linked `imported` address-book row carrying pending (and
   * optional dismissed) method suggestions, plus the CRM contact it links
   * to. Used to drive the method-suggestion card on the People tab.
   */
  async seedMethodSuggestions(
    input: Omit<SeedMethodSuggestionsRequest, 'prefix'>
  ): Promise<SeedMethodSuggestionsResponse> {
    const response = await this.request.post(
      `${API_BASE_URL}/api/v1/test/seed/method-suggestions`,
      {
        headers: API_HEADERS,
        data: {
          prefix: this.prefix,
          ...input,
        } satisfies SeedMethodSuggestionsRequest,
      }
    )

    if (!response.ok()) {
      const body = await response.text()
      throw new Error(`Failed to seed method suggestions: ${response.status()} ${body}`)
    }

    const data = await response.json()
    return data.data as SeedMethodSuggestionsResponse
  }

  /**
   * Seeds contacts with backdated last_contacted timestamps so they appear as overdue.
   * Useful for testing dashboard and overdue contact lists.
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
   * Seeds calendar events linked to a contact.
   * Useful for testing the Meetings component on contact pages.
   */
  async seedCalendarEvents(
    contactId: string,
    events: SeedCalendarEventInput[]
  ): Promise<SeedCalendarEventsResponse> {
    const response = await this.request.post(`${API_BASE_URL}/api/v1/test/seed/calendar-events`, {
      headers: API_HEADERS,
      data: {
        prefix: this.prefix,
        contact_id: contactId,
        events,
      } satisfies SeedCalendarEventsRequest,
    })

    if (!response.ok()) {
      const body = await response.text()
      throw new Error(`Failed to seed calendar events: ${response.status()} ${body}`)
    }

    const data = await response.json()
    return data.data as SeedCalendarEventsResponse
  }

  /**
   * Deletes every existing mac_host so the singleton unique index is free
   * before seeding a fresh host. The mac_host table allows only one host
   * at a time, so paired-host tests must run serially and reset first.
   */
  async resetMacHosts(): Promise<void> {
    const resp = await this.request.get(`${API_BASE_URL}/api/v1/host`, { headers: API_HEADERS })
    if (!resp.ok()) return
    const json = (await resp.json()) as { data?: Array<{ id: string }> }
    for (const host of json.data ?? []) {
      await this.request.delete(`${API_BASE_URL}/api/v1/host/${host.id}`, { headers: API_HEADERS })
    }
  }

  /**
   * Seeds a paired mac_host row and remembers its id so cleanup() can scope
   * meeting_note deletion to it. Returns the host id.
   */
  async seedMacHost(req: SeedMacHostRequest = {}): Promise<string> {
    const response = await this.request.post(`${API_BASE_URL}/api/v1/test/seed/mac-hosts`, {
      headers: API_HEADERS,
      data: {
        hostname: req.hostname ?? `${this.prefix}-host`,
        protocol_version: req.protocol_version ?? 1,
        ...req,
      },
    })

    if (!response.ok()) {
      const body = await response.text()
      throw new Error(`Failed to seed mac host: ${response.status()} ${body}`)
    }

    const data = await response.json()
    const hostId = (data.data as SeedMacHostResponse).host_id
    this._seededHostId = hostId
    return hostId
  }

  /**
   * Seeds orphan meeting_note rows against a paired host so the Imports
   * Interactions tab has rows to render. Caller supplies the session UUIDs
   * (used by the ?session deep-link). Cleanup is by host id.
   */
  async seedMeetingNotes(
    hostId: string,
    notes: SeedMeetingNoteInput[]
  ): Promise<SeedMeetingNotesResponse> {
    const response = await this.request.post(`${API_BASE_URL}/api/v1/test/seed/meeting-notes`, {
      headers: API_HEADERS,
      data: { host_id: hostId, notes } satisfies SeedMeetingNotesRequest,
    })

    if (!response.ok()) {
      const body = await response.text()
      throw new Error(`Failed to seed meeting notes: ${response.status()} ${body}`)
    }

    const data = await response.json()
    return data.data as SeedMeetingNotesResponse
  }

  /**
   * Seeds a note (notepad) for a contact.
   * Notes are stored in a separate note table with category='notepad'.
   * Useful for testing notes display and editing on contact pages.
   */
  async seedContactNote(contactId: string, body: string): Promise<void> {
    const response = await this.request.put(`${API_BASE_URL}/api/v1/contacts/${contactId}/notes`, {
      headers: API_HEADERS,
      data: { body },
    })

    if (!response.ok()) {
      const responseBody = await response.text()
      throw new Error(`Failed to seed contact note: ${response.status()} ${responseBody}`)
    }
  }

  /**
   * Seeds the fixture DECLARED for a spec behavior and returns its manifest.
   *
   * Each call gets its own namespace (`${prefix}-c${n}`): the generator is a
   * pure function of (seed, namespace), so two calls sharing one namespace
   * would mint identical names and emails and collide.
   *
   * The namespace is recorded LOCALLY BEFORE the request is issued. That is the
   * durable cleanup handle: if the response never arrives, the rows may still
   * exist, and this token is the only trace the client has of them. The server
   * expands a requested token to whatever it actually created, so cleanup works
   * even when the client never learned the effective namespace.
   */
  async seedBehavior(behaviorId: string): Promise<SeedBehaviorResult> {
    const namespace = `${this._prefix}-c${++this._seedBehaviorCalls}`
    this._declaredNamespaces.push(namespace)

    const response = await this.request.post(`${API_BASE_URL}/api/v1/test/seed/declared`, {
      headers: API_HEADERS,
      data: { behavior_id: behaviorId, namespace },
    })

    const body = await response.text()
    let parsed: { data?: { namespace?: string; entities?: Record<string, SeededEntity> } } = {}
    try {
      parsed = JSON.parse(body)
    } catch {
      // Non-JSON body: nothing to learn from it beyond the status.
    }
    // Record the effective namespace whether the call succeeded or failed — a
    // failed seed can still have left a partial world under a re-salted name.
    const effective = parsed?.data?.namespace
    if (effective && !this._declaredNamespaces.includes(effective)) {
      this._declaredNamespaces.push(effective)
    }

    if (!response.ok()) {
      throw new Error(`Failed to seed behavior ${behaviorId}: ${response.status()} ${body}`)
    }
    return parsed.data as SeedBehaviorResult
  }

  /**
   * Cleans up all test data created with this test's prefix, plus every
   * declared namespace this test seeded.
   * Call this in afterEach or afterAll to ensure test isolation.
   *
   * The two sweeps are INDEPENDENT and both always run. They delete disjoint
   * rows by unrelated mechanisms, so a failure of one says nothing about the
   * other — and returning early on the prefix sweep's error (a transient 500, a
   * dropped request) would leave every declared world this test seeded alive in
   * the shared E2E database, compounding one failing test into an isolation
   * failure for everything that runs after it. Both errors are reported.
   */
  async cleanup(): Promise<CleanupResponse> {
    let prefixResult: CleanupResponse | undefined
    let prefixError: unknown
    try {
      const response = await this.request.post(`${API_BASE_URL}/api/v1/test/cleanup`, {
        headers: API_HEADERS,
        data: {
          prefix: this.prefix,
          ...(this._seededHostId ? { host_id: this._seededHostId } : {}),
        } satisfies CleanupRequest,
      })
      if (!response.ok()) {
        throw new Error(
          `Failed to cleanup test data: ${response.status()} ${await response.text()}`
        )
      }
      prefixResult = (await response.json()).data as CleanupResponse
    } catch (error) {
      prefixError = error
    }

    let declaredError: unknown
    try {
      await this.cleanupDeclaredNamespaces()
    } catch (error) {
      declaredError = error
    }

    if (prefixError || declaredError) {
      throw new Error(
        [prefixError, declaredError]
          .filter(Boolean)
          .map(error => (error instanceof Error ? error.message : String(error)))
          .join('\n')
      )
    }
    return prefixResult as CleanupResponse
  }

  /**
   * Removes the declared worlds this test seeded. `busy` and `pending` mean the
   * server refused to delete anything yet (a seed is in flight, or a background
   * job still references the rows) and are retriable, so they are POLLED until
   * they resolve or the budget runs out, then failed loudly — silently leaving
   * rows behind would poison the shared database for later runs.
   */
  private async cleanupDeclaredNamespaces(): Promise<void> {
    if (this._declaredNamespaces.length === 0) return

    const post = async (namespaces: string[]): Promise<CleanupNamespacesResponse> => {
      const response = await this.request.post(`${API_BASE_URL}/api/v1/test/cleanup`, {
        headers: API_HEADERS,
        data: { namespaces },
      })
      const body = await response.text()
      if (!response.ok()) {
        throw new Error(`Failed to clean declared namespaces: ${response.status()} ${body}`)
      }
      return JSON.parse(body).data as CleanupNamespacesResponse
    }

    // Batched: the server rejects an oversized list outright, and that 400
    // arrives before it has deleted anything, so one long-running test could
    // strand every world it seeded.
    const attempt = async (namespaces: string[]): Promise<CleanupNamespacesResponse> => {
      const merged: CleanupNamespacesResponse = { expansions: {}, results: {} }
      for (let i = 0; i < namespaces.length; i += DECLARED_CLEANUP_MAX_PER_REQUEST) {
        const batch = await post(namespaces.slice(i, i + DECLARED_CLEANUP_MAX_PER_REQUEST))
        Object.assign(merged.expansions, batch.expansions)
        Object.assign(merged.results, batch.results)
      }
      return merged
    }

    const retriable = (result: CleanupNamespacesResponse) =>
      Object.entries(result.results)
        .filter(([, outcome]) => outcome.status === 'busy' || outcome.status === 'pending')
        .map(([namespace]) => namespace)

    // Outcomes accumulate ACROSS attempts, because a retry only names the
    // namespaces that were retriable. A namespace that came back with a
    // non-retriable `error` is absent from every later response, so replacing
    // the result wholesale would report success while its residue is still
    // there. Re-sent tokens are dropped first: expansion can move a token's
    // verdict to a different key between attempts (a requested token whose run
    // had not published its marker yet answers under ITSELF, and under
    // `<ns>-sN` once that run finishes and the salted world becomes
    // discoverable), so keeping the earlier entry would report a phantom
    // `busy` for a world that has since been swept.
    const outcomes = new Map<string, NamespaceCleanupOutcome>()
    let sending = this._declaredNamespaces
    const deadline = Date.now() + DECLARED_CLEANUP_POLL_BUDGET_MS

    for (;;) {
      const result = await attempt(sending)
      for (const namespace of sending) outcomes.delete(namespace)
      for (const [namespace, outcome] of Object.entries(result.results)) {
        outcomes.set(namespace, outcome)
      }

      sending = retriable(result)
      if (sending.length === 0) break
      if (Date.now() + DECLARED_CLEANUP_POLL_INTERVAL_MS >= deadline) break
      this.grantDeclaredCleanupBudget()
      await new Promise(resolve => setTimeout(resolve, DECLARED_CLEANUP_POLL_INTERVAL_MS))
    }

    const failures = [...outcomes].filter(([, outcome]) => outcome.status !== 'cleaned')
    if (failures.length > 0) {
      throw new Error(
        'Declared namespace cleanup left rows behind (retriable outcomes were polled for up to ' +
          `${DECLARED_CLEANUP_POLL_BUDGET_MS}ms): ${JSON.stringify(Object.fromEntries(failures))}`
      )
    }
  }

  /**
   * Buys the cleanup poll the wall-clock it needs, once, on the path that
   * actually polls.
   *
   * afterEach hooks share the TEST's timeout slot (Playwright's TimeoutManager
   * charges hook time against the same budget), and the declared-seed run this
   * poll is waiting out can hold its reservation for minutes. Without this the
   * poll's bound is a bound in name only: the hook is killed part way through
   * and the rows survive anyway. A timeout of 0 means timeouts are disabled for
   * this test — setting one would IMPOSE a ceiling that is not there.
   *
   * It cannot help a test that had already exhausted its slot before afterEach
   * started; that hook is rejected on entry, before any client code runs.
   */
  private grantDeclaredCleanupBudget(): void {
    if (this._cleanupBudgetGranted) return
    this._cleanupBudgetGranted = true
    if (this.testInfo.timeout > 0) {
      this.testInfo.setTimeout(this.testInfo.timeout + DECLARED_CLEANUP_POLL_BUDGET_MS)
    }
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
