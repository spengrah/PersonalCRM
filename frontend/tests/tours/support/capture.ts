// The capture engine. Emits one parseable capture record per capture() call: a
// structured aria node tree, self-describing endpoint-grouped /api/v1 responses,
// the accelerated serverTime frame, native dialogs, and optional probes/fields —
// with conservative deterministic normalization (normalize.ts).

import * as fs from 'fs'
import * as path from 'path'
import type {
  APIRequestContext,
  APIResponse,
  Page,
  Request,
  Response,
  TestInfo,
} from '@playwright/test'
import {
  CAPTURE_FORMAT_VERSION,
  type ApiProbe,
  type ApiResponseItem,
  type ApiResponses,
  type Capture,
  type CaptureOptions,
  type DialogRecord,
  type ServerTimeFrame,
} from './types'
import {
  createUuidMapper,
  DEFAULT_ARIA_CAP,
  DEFAULT_ARRAY_CAP,
  endpointKey,
  normalizeAriaTree,
  normalizeJson,
  normalizeUrl,
  parseAriaSnapshot,
  parseQuery,
  type UuidMapper,
} from './normalize'
import { capturesDir, getCurrentRunId } from './run-dir'

const MUTATING = new Set(['POST', 'PUT', 'PATCH'])

type PathPattern = string | RegExp
type RouteMatcher = (url: URL) => boolean

export interface WithDialogOptions {
  accept: boolean
  timeoutMs?: number
}

export interface RouteHold {
  release: () => Promise<void>
}

function slugify(note: string): string {
  return (
    note
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '')
      .slice(0, 60) || 'capture'
  )
}

// One TourApi per test (a tour is a single serial test). Its UUID mapper is
// shared across every capture in the tour so before/after pairs correlate. The
// response buffer + listener are set up by the fixture.
export class TourApi {
  private seq = 0
  private readonly uuid: UuidMapper = createUuidMapper()
  private readonly tour: string
  private readonly pendingDialogs: DialogRecord[] = []

  constructor(
    readonly apiCtx: APIRequestContext,
    private readonly buffer: Response[],
    testInfo: TestInfo
  ) {
    this.tour = path.basename(testInfo.file).replace(/\.tour\.ts$/, '')
  }

  // Write one capture record. Every call site must first pass an explicit
  // readiness gate so the response buffer is complete.
  async capture(page: Page, opts: CaptureOptions): Promise<void> {
    const arrayCap = opts.arrayCap ?? DEFAULT_ARRAY_CAP
    const ariaCap = opts.ariaCap ?? DEFAULT_ARIA_CAP
    const seq = ++this.seq

    const url = normalizeUrl(page.url(), this.uuid)
    const ariaRoot = opts.ariaRoot ?? page.locator('body')
    const ariaYaml = await ariaRoot.ariaSnapshot()
    const aria = normalizeAriaTree(parseAriaSnapshot(ariaYaml), this.uuid, ariaCap)

    const apiResponses: ApiResponses = {}
    // Drain everything buffered since the previous capture; record EVERY item.
    const drained = this.buffer.splice(0)
    for (const resp of drained) {
      await this.recordResponse(apiResponses, resp, arrayCap)
    }
    for (const probe of opts.probes ?? []) {
      await this.recordProbe(apiResponses, probe, arrayCap)
    }

    const serverTime = await this.fetchServerTime()
    const fields = opts.fields
      ? (normalizeJson(opts.fields, this.uuid, arrayCap) as Record<string, unknown>)
      : undefined
    const dialogs = this.pendingDialogs.splice(0)

    const record: Capture = {
      captureFormatVersion: CAPTURE_FORMAT_VERSION,
      tour: this.tour,
      seq,
      behaviors: opts.behaviors,
      note: opts.note,
      url,
      pair: opts.pair ?? null,
      serverTime,
      aria,
      apiResponses,
      ...(fields ? { fields } : {}),
      dialogs,
    }
    this.write(seq, opts.note, record)
  }

  // Register a native-dialog handler: record the message into the pending
  // capture's dialogs[], accept/dismiss per the flag, await both the dialog and
  // the triggering action, and throw loudly if no dialog fires in time (a
  // delete flow that no longer prompts is a real regression, not to be
  // swallowed). page.waitForEvent auto-removes its listener.
  async withDialog(
    page: Page,
    opts: WithDialogOptions,
    action: () => Promise<void>
  ): Promise<void> {
    const { accept, timeoutMs = 5000 } = opts
    const dialogPromise = page.waitForEvent('dialog', { timeout: timeoutMs }).then(async dialog => {
      this.pendingDialogs.push({ type: dialog.type(), message: dialog.message() })
      if (accept) await dialog.accept()
      else await dialog.dismiss()
    })
    const results = await Promise.allSettled([dialogPromise, Promise.resolve().then(action)])
    for (const r of results) {
      if (r.status === 'rejected') throw r.reason
    }
  }

  // Readiness sugar: resolve on the matching buffered response, else wait for
  // the next one — NOT expect(), so tours stay assertion-free.
  async waitForApi(
    page: Page,
    method: string,
    pathPattern: PathPattern,
    opts: { timeout?: number } = {}
  ): Promise<Response> {
    const timeout = opts.timeout ?? 15000
    const matches = (r: Response): boolean =>
      r.request().method() === method && this.matchPath(r.url(), pathPattern)
    const existing = this.buffer.find(matches)
    if (existing) return existing
    return page.waitForResponse(matches, { timeout })
  }

  // Deterministically hold a route until release() (for capturing a
  // loading/in-flight disabled state without timing luck).
  // The handler continues each intercepted route exactly once after the gate
  // opens; we deliberately do NOT page.unroute() on release — that races the
  // held route's continue and throws "Route is already handled". Leaving the
  // registration is harmless once the gate is open (later matching requests
  // continue immediately) and it is torn down when the page closes.
  async holdRoute(page: Page, matcher: RouteMatcher): Promise<RouteHold> {
    let releaseGate!: () => void
    const gate = new Promise<void>(resolve => {
      releaseGate = resolve
    })
    let released = false
    await page.route(matcher, async route => {
      await gate
      await route.continue()
    })
    return {
      release: async () => {
        if (released) return
        released = true
        releaseGate()
      },
    }
  }

  private matchPath(url: string, pattern: PathPattern): boolean {
    let pathname: string
    try {
      pathname = new URL(url).pathname
    } catch {
      pathname = url
    }
    return typeof pattern === 'string' ? pathname.includes(pattern) : pattern.test(pathname)
  }

  private async recordResponse(map: ApiResponses, resp: Response, arrayCap: number): Promise<void> {
    const req = resp.request()
    const method = req.method()
    const fullUrl = resp.url()
    const item: ApiResponseItem = {
      method,
      requestUrl: normalizeUrl(fullUrl, this.uuid),
      query: parseQuery(fullUrl, this.uuid),
      status: resp.status(),
      body: await this.readBody(resp, arrayCap),
    }
    if (MUTATING.has(method)) {
      const requestBody = this.readRequestBody(req, arrayCap)
      if (requestBody !== undefined) item.requestBody = requestBody
    }
    const key = endpointKey(method, fullUrl)
    ;(map[key] ??= []).push(item)
  }

  private async recordProbe(map: ApiResponses, probe: ApiProbe, arrayCap: number): Promise<void> {
    const resp = await this.apiCtx.fetch(probe.path, { method: probe.method })
    const item: ApiResponseItem = {
      method: probe.method,
      requestUrl: normalizeUrl(probe.path, this.uuid),
      query: parseQuery(probe.path, this.uuid),
      status: resp.status(),
      body: await this.readBody(resp, arrayCap),
      probe: true,
    }
    const key = endpointKey(probe.method, probe.path)
    ;(map[key] ??= []).push(item)
  }

  private async readBody(resp: Response | APIResponse, arrayCap: number): Promise<unknown> {
    try {
      const json = await resp.json()
      return normalizeJson(json, this.uuid, arrayCap)
    } catch {
      // Empty / non-JSON (e.g. DELETE → 204) — recorded as null, never dropped.
      return null
    }
  }

  private readRequestBody(req: Request, arrayCap: number): unknown {
    try {
      const data = req.postDataJSON()
      if (data === null || data === undefined) return undefined
      return normalizeJson(data, this.uuid, arrayCap)
    } catch {
      return undefined
    }
  }

  // The authoritative accelerated frame: fetched via the dedicated apiCtx (NOT
  // the page context) so it is not caught by the response listener. Every
  // capture must be stamped with this frame — a failed or malformed fetch
  // THROWS to fail the sweep loudly rather than emit a frame-less capture.
  private async fetchServerTime(): Promise<ServerTimeFrame> {
    const resp = await this.apiCtx.get('/api/v1/system/time')
    if (!resp.ok()) {
      throw new Error(
        `tours: GET /api/v1/system/time returned ${resp.status()} — cannot stamp the capture with a server-time frame`
      )
    }
    let envelope: ({ data?: Record<string, unknown> } & Record<string, unknown>) | undefined
    try {
      envelope = await resp.json()
    } catch {
      throw new Error('tours: GET /api/v1/system/time returned a non-JSON body')
    }
    const data = (envelope?.data ?? envelope) as Record<string, unknown> | undefined
    if (!data || !data.current_time) {
      throw new Error('tours: GET /api/v1/system/time response is missing current_time')
    }
    return {
      currentTime: String(data.current_time),
      isAccelerated: Boolean(data.is_accelerated),
      accelerationFactor: Number(data.acceleration_factor ?? 1),
      baseTime: String(data.base_time ?? ''),
      environment: data.environment ? String(data.environment) : undefined,
    }
  }

  private write(seq: number, note: string, record: Capture): void {
    const runId = getCurrentRunId()
    const dir = capturesDir(runId, this.tour)
    fs.mkdirSync(dir, { recursive: true })
    const file = path.join(dir, `${String(seq).padStart(3, '0')}-${slugify(note)}.json`)
    fs.writeFileSync(file, `${JSON.stringify(record, null, 2)}\n`, 'utf8')
  }
}
