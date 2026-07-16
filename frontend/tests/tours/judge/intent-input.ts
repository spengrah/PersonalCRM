// Bind an intent's evidence and build its judge input. An intent's evidence is
// the union of captures tagged with the intent's own ID or with any behavior in
// its servedBy set (the inverted `serves:` edges from the SSOT), deduped,
// ordered by (tour, seq), and capped — the dropped count is surfaced, never
// silently truncated. Pure — no model, no fs.

import type { Capture } from '../support/types'
import type { LoadedCapture } from '../support/run-dir'
import type { CaptureSection, JudgeInput } from './adapter/types'
import type { IntentSpec } from './intent-catalog'

// Default per-intent capture cap: multi-capture prompts carry full aria trees,
// so an unbounded union can blow the prompt budget. 8 keeps the common tours
// intact; the report logs what was dropped.
export const INTENT_CAPTURE_CAP = 8

export interface BoundIntentCaptures {
  captures: Capture[]
  dropped: number
}

export function bindIntentCaptures(
  intent: IntentSpec,
  all: Capture[],
  cap: number = INTENT_CAPTURE_CAP
): BoundIntentCaptures {
  const wanted = new Set([intent.id, ...intent.servedBy])
  const seen = new Set<string>()
  const bound: Capture[] = []
  for (const c of all) {
    if (!c.behaviors.some(b => wanted.has(b))) continue
    const key = `${c.tour}#${c.seq}`
    if (seen.has(key)) continue
    seen.add(key)
    bound.push(c)
  }
  bound.sort((a, b) => (a.tour === b.tour ? a.seq - b.seq : a.tour < b.tour ? -1 : 1))
  const kept = bound.slice(0, cap)
  return { captures: kept, dropped: bound.length - kept.length }
}

// One CAPTURE[n] section per bound capture — evidence stays sectioned (the
// behavior path's merged-aria bundle would blur which state showed what).
export function captureSection(c: Capture): CaptureSection {
  return {
    // The REAL source filename when the loader stamped it (the report walk sets
    // `LoadedCapture.__sourceFile`); otherwise a VISIBLE fallback so a missing
    // filename is never passed off as one. This is the sole place the source
    // identity is projected onto the adapter-visible `CaptureSection` (D3).
    captureFile: (c as LoadedCapture).__sourceFile ?? `unknown:${c.tour}/${c.seq}`,
    note: `${c.tour}#${c.seq} — ${c.note}`,
    evidence: {
      url: c.url,
      aria: c.aria,
      api: c.apiResponses,
      serverTime: c.serverTime,
      dialogs: c.dialogs,
    },
  }
}

/**
 * Resolve a bound capture to an attachable screenshot file (absolute path),
 * or undefined. Impure concerns (run-dir layout, fs existence) live in the
 * caller-supplied resolver so this module stays pure.
 */
export type ScreenshotResolver = (c: Capture) => string | undefined

export function buildIntentJudgeInput(
  intent: IntentSpec,
  bound: Capture[],
  resolveScreenshot?: ScreenshotResolver
): JudgeInput {
  // ALL-OR-NOTHING: the prompt promises images in CAPTURE[n] order, so a gap
  // (one capture's best-effort screenshot missing) would silently shift every
  // later image onto the wrong capture — a miscited visual verdict. Any gap
  // drops ALL images: the run degrades to aria-only framing + the visual
  // caveat instead of corrupting the mapping.
  const resolved = resolveScreenshot ? bound.map(resolveScreenshot) : []
  const images =
    resolved.length > 0 && resolved.every((p): p is string => p !== undefined) ? resolved : []
  return {
    behaviorId: intent.id,
    behaviorTitle: intent.title,
    given: '',
    when: '',
    then: [intent.statement],
    items: [{ itemIndex: 0, thenText: intent.statement }],
    evidence: {},
    intent: { statement: intent.statement, status: intent.status },
    captureSections: bound.map(captureSection),
    ...(images.length > 0 ? { images } : {}),
  }
}
