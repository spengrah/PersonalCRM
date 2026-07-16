// Shared PII pattern definitions for the Langfuse export scrub (scrub.ts, which
// replaces matches on the way out to Langfuse). Email + phone are the free-form
// identifiers a live-run trace can carry; everything else in a shipped body is
// identifier/enum-valued by construction (see the INV-2 note at the scrub seam).

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
