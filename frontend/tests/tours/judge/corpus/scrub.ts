// Corpus-only scrub (design D6a P1): the live-run normalizer host-scrubs URLs +
// UUID-maps ids, but does NOT redact emails/phones — and the synthetic factory
// emits real-FORMAT fabricated emails/phones into methods[].value. This scrub
// maps every email-shaped and phone-shaped string to a stable placeholder
// (<email:N>/<phone:N>, first-seen like the UUID mapper), applied to every
// capture AT CURATION on top of the normalizer, so committed fixtures carry no
// email/phone patterns. Evidence-preserving: no toured behavior's verifier
// reads a method value (CON-043 uses names, CON-044 the interaction, CON-045
// birthdays).
//
// Pure — no Playwright, no fs. Deep-walks a parsed capture object.

import { EMAIL_RE, PHONE_RES } from './patterns'

export interface Scrubber {
  scrub(value: string): string
}

// A first-seen map so the same email/phone → the same placeholder across every
// capture in a curation session (structure-preserving, PII-safe).
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

// Scrub a parsed capture record (returns a new object).
export function scrubCapture<T>(capture: T, scrubber: Scrubber = createScrubber()): T {
  return scrubValue(capture, scrubber) as T
}
