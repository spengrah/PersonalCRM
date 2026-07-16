// Shared capture-format contract for the tours harness.
//
// This is the single load-bearing interface: the tours produce these records
// and the downstream grader + judge consume them. It is versioned via
// captureFormatVersion / captureGeneratorVersion — bump both on any schema
// change. The only Playwright reference is the type-only `Locator` import below
// (erased at compile time — the pure normalizer and its vitest unit tests never
// load a browser runtime).

import type { Locator } from '@playwright/test'

export const CAPTURE_FORMAT_VERSION = 1
export const CAPTURE_GENERATOR_VERSION = 1

// A single accessibility node parsed from Playwright's ariaSnapshot() YAML.
// Element nodes carry a role, an optional accessible name, per-node state (only
// when its token is present), and children. Visible copy is preserved as leaf
// text nodes: { role: 'text', text }.
export interface AriaNode {
  role: string
  /** Accessible name (element nodes) — UUID-mapped, host-redacted. */
  name?: string
  /** Verbatim visible copy (role === 'text' leaves) — UUID-mapped. */
  text?: string
  disabled?: boolean
  checked?: boolean | 'mixed'
  expanded?: boolean
  level?: number
  pressed?: boolean | 'mixed'
  selected?: boolean
  active?: boolean
  children?: AriaChild[]
}

// A sibling-cap marker inserted when a parent has more children than ariaCap.
export interface AriaTruncationMarker {
  __ariaTruncated__: number
}

export type AriaChild = AriaNode | AriaTruncationMarker

// An array-cap marker inserted when a normalized JSON array exceeds arrayCap.
export interface JsonTruncationMarker {
  __truncated__: number
}

// A single self-describing /api/v1 response item. Items are grouped under a
// templated endpoint key (e.g. "GET /api/v1/contacts/:id"); the requestUrl +
// parsed query disambiguate otherwise-identical keys. Mutating requests also
// carry a normalized requestBody. `probe: true` flags an item obtained by a
// harness-initiated fetch, not observed on the page.
export interface ApiResponseItem {
  method: string
  requestUrl: string
  query: Record<string, string>
  status: number
  // Parsed + normalized JSON body, or null for an empty / non-JSON body
  // (e.g. a 204). The item + its status is never dropped.
  body: unknown
  requestBody?: unknown
  probe?: boolean
}

export type ApiResponses = Record<string, ApiResponseItem[]>

// The accelerated server-time frame from GET /api/v1/system/time. Preserved raw
// — it is the interpretation anchor for time-dependent evidence. Every capture
// is stamped with one (a failed fetch fails the sweep loudly).
export interface ServerTimeFrame {
  currentTime: string
  isAccelerated: boolean
  accelerationFactor: number
  baseTime: string
  environment?: string
}

// Groups an ordered sequence of captures (by seq) that the grader diffs as one
// bracket. `role` is a free label, NOT an enum.
export interface CapturePair {
  id: string
  role: string
}

// A native browser dialog observed during a capture's bracketed action.
export interface DialogRecord {
  type: string
  message: string
}

// One capture record — one JSON object per capture() call. Both version fields
// are stamped so a single capture file is self-describing (the manifest also
// carries captureGeneratorVersion at the run level).
export interface Capture {
  captureFormatVersion: number
  captureGeneratorVersion: number
  tour: string
  seq: number
  behaviors: string[]
  note: string
  url: string
  pair: CapturePair | null
  serverTime: ServerTimeFrame
  aria: AriaNode
  apiResponses: ApiResponses
  fields?: Record<string, unknown>
  dialogs: DialogRecord[]
  /**
   * Run-dir-relative path of the capture-point screenshot (e.g.
   * screenshots/dashboard/004-....png). Live-run evidence for the intent
   * judge's visual goals — lives only in the gitignored run dir, never git (the
   * export scrub greps JSON, not pixels). Absent when screenshots are disabled
   * or the best-effort capture failed.
   */
  screenshot?: string
}

// The run manifest. The staging host is redacted.
export interface Manifest {
  captureFormatVersion: number
  captureGeneratorVersion: number
  gitSha: string
  stagingImageDigest: string
  seedProfile: string
  baseUrl: string
  timestamp: string
  runId: string
}

// Options accepted by capture(). `behaviors` + `note` are required; everything
// else is optional per-capture tuning. `ariaRoot` is an authoring-time locator,
// NOT part of the emitted record.
export interface CaptureOptions {
  behaviors: string[]
  note: string
  pair?: CapturePair
  /** Harness-initiated fetches recorded into apiResponses with probe: true. */
  probes?: ApiProbe[]
  /** Named structured values read directly (e.g. an input's live value). */
  fields?: Record<string, unknown>
  /** Response-array cap (default 50; Infinity preserves the full array). */
  arrayCap?: number
  /** Aria sibling cap (default 20; Infinity preserves all siblings). */
  ariaCap?: number
  /**
   * Element to root the aria snapshot at (default: the page body). Scope to an
   * overlay (e.g. a modal) so its focused subtree is not truncated out of a
   * content-rich body by ariaCap.
   */
  ariaRoot?: Locator
}

export interface ApiProbe {
  method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  path: string
}
