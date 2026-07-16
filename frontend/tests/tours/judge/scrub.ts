// Email/phone scrubber for the Langfuse export path (arc INV-2): every
// free-form string that leaves for Langfuse is scrubbed HERE, at the export
// boundary, so live captures never ship real-FORMAT emails/phones (the synthetic
// factory emits `+1-479-555-0100`, `<name>@…`). Maps every email-shaped and
// phone-shaped string to a stable first-seen placeholder (<email:N>/<phone:N>).
//
// The placeholders (<email:N>/<phone:N>) do NOT match the email/phone REs, so a
// second scrub pass is a no-op — the seam is safe to sit anywhere without
// corrupting an already-scrubbed string.
//
// Pure — no Playwright, no fs. Deep-walks a parsed value via scrubValue.

import { EMAIL_RE, PHONE_RES } from './pii-patterns'

export interface Scrubber {
  scrub(value: string): string
}

// A first-seen map so the same email/phone → the same placeholder across every
// string in a single scrub session (structure-preserving, PII-safe). One
// scrubber per export run keeps placeholders stable within that export.
export function createScrubber(): Scrubber {
  const emails = new Map<string, string>()
  const phones = new Map<string, string>()
  const placeholder = (map: Map<string, string>, kind: string, key: string): string => {
    let p = map.get(key)
    if (!p) {
      p = `<${kind}:${map.size + 1}>`
      map.set(key, p)
    }
    return p
  }
  return {
    scrub(value: string): string {
      let out = value.replace(EMAIL_RE, m => placeholder(emails, 'email', m.toLowerCase()))
      for (const re of PHONE_RES) {
        out = out.replace(re, m => placeholder(phones, 'phone', m.replace(/\s+/g, '')))
      }
      return out
    },
  }
}

// Deep-walk a JSON value, scrubbing every string. Arrays/objects rebuilt so the
// input is not mutated.
export function scrubValue(value: unknown, scrubber: Scrubber): unknown {
  if (typeof value === 'string') return scrubber.scrub(value)
  if (Array.isArray(value)) return value.map(v => scrubValue(v, scrubber))
  if (value !== null && typeof value === 'object') {
    const out: Record<string, unknown> = {}
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      out[k] = scrubValue(v, scrubber)
    }
    return out
  }
  return value
}
