// Shared PII pattern definitions — the SINGLE source used by BOTH the corpus
// scrub (scrub.ts, which replaces matches) and the PII audit (pii-audit.ts,
// which bans them). Keeping one source guarantees the audit bans exactly what
// the scrub removes.

// Email addresses.
export const EMAIL_RE = /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g

// Phone numbers. Chosen to catch the synthetic factory's formats
// (+1-479-555-0100) and common real formats WITHOUT matching preserved
// evidence: ISO dates are YYYY-MM-DD (4-2-2 groups) and timestamps use ':'/'.',
// neither of which matches the NANP 3-3-4 shape or the leading-'+' form.
export const PHONE_RES: RegExp[] = [
  /\+\d[\d().\-\s]{6,}\d/g, // E.164 / international (leading +)
  /\(\d{3}\)[\s.\-]?\d{3}[\s.\-]?\d{4}/g, // (479) 555-0100
  /\b\d{3}[.\-\s]\d{3}[.\-\s]\d{4}\b/g, // 479-555-0100 (3-3-4, not 4-2-2 dates)
]

// A raw contact/entity UUID (must have been mapped to <id:N> before commit).
export const RAW_UUID_RE = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi

// An absolute URL with a REAL host — i.e. https?:// NOT followed by the safe
// placeholders the normalizer/manifest emit (<host> / <staging>).
export const REAL_HOST_URL_RE = /https?:\/\/(?!<host>|<staging>)[^\s"'<>)\]}]+/gi

// Obvious secret/token literals (defense-in-depth; the normalizer already
// sentinels secret-bearing KEYS to <redacted>, this catches stray VALUES).
export const SECRET_RES: RegExp[] = [
  /\bBearer\s+[A-Za-z0-9._\-]{8,}/g,
  /\beyJ[A-Za-z0-9._\-]{10,}/g, // JWT
  /\bsk-[A-Za-z0-9]{16,}/g, // OpenAI-style secret key
]

// The mechanical synthetic-provenance prefix every prod-shaped contact name
// carries (factory.Prefix() = synth-<namespace>-). A real target's un-prefixed
// names fail the audit regardless of any manifest label.
export const SYNTH_NAME_PREFIX = 'synth-prodshaped-'
