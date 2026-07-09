// Shared capture-format contract for the tours harness (Piece 4 Track B, #606).
//
// This is the single load-bearing interface of the arc: PR1 produces it, and
// PR2's hybrid grader + PR3's corpus consume it. The schema is authored here
// per arc §1 and versioned via captureFormatVersion / captureGeneratorVersion —
// any schema change bumps both (arc §3). Keep this file free of any Playwright
// import so the pure normalizer + its unit tests can consume it without a
// browser runtime.

export const CAPTURE_FORMAT_VERSION = 1
export const CAPTURE_GENERATOR_VERSION = 1

// A single accessibility node parsed from Playwright's ariaSnapshot() YAML
// (arc §1d). Element nodes carry a role, an optional accessible name, per-node
// state (only when its token is present), and children. Visible copy is
// preserved as leaf text nodes: { role: 'text', text }.
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

// A single self-describing /api/v1 response item (arc §1a). Items are grouped
// under a templated endpoint key (e.g. "GET /api/v1/contacts/:id"); the
// requestUrl + parsed query disambiguate otherwise-identical keys. Mutating
// requests also carry a normalized requestBody. `probe: true` flags an item
// obtained by a harness-initiated fetch (D4), not observed on the page.
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

// The accelerated server-time frame from GET /api/v1/system/time (arc §1a).
// Preserved raw — it is the interpretation anchor for time-dependent evidence.
export interface ServerTimeFrame {
  currentTime: string
  isAccelerated: boolean
  accelerationFactor: number
  baseTime: string
  environment?: string
}

// Groups an ordered sequence of captures (by seq) that the grader diffs as one
// bracket. `role` is a free label, NOT an enum (arc §1a).
export interface CapturePair {
  id: string
  role: string
}

// A native browser dialog observed during a capture's bracketed action (D5).
export interface DialogRecord {
  type: string
  message: string
}

// One capture record — one JSON object per capture() call (arc §1a).
export interface Capture {
  captureFormatVersion: number
  tour: string
  seq: number
  behaviors: string[]
  note: string
  url: string
  pair: CapturePair | null
  serverTime: ServerTimeFrame | null
  aria: AriaNode
  apiResponses: ApiResponses
  fields?: Record<string, unknown>
  dialogs: DialogRecord[]
}

// The run manifest (arc §1b). The staging host is redacted.
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
// else is optional per-capture tuning (arc §1a/§1c-5).
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
}

export interface ApiProbe {
  method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  path: string
}
