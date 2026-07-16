// Corpus-only curation wrapper (design D6a P1). The scrubber itself now lives in
// the export path (`judge/scrub.ts`, arc INV-2) — this file keeps ONLY the
// curation-specific `scrubCapture`, which additionally drops the live-run-only
// `screenshot` path so committed fixtures stay aria-only. Used at CURATION on
// top of the normalizer, so committed fixtures carry no email/phone patterns.
// (Retires with the corpus in a later arc PR.)

import { createScrubber, scrubValue, type Scrubber } from '../scrub'

// Scrub a parsed capture record (returns a new object). Also drops the
// live-run-only `screenshot` path: it references a gitignored file that never
// enters the corpus, so committing it would be a dangling pointer (the
// committed corpus stays aria-only by design).
export function scrubCapture<T>(capture: T, scrubber: Scrubber = createScrubber()): T {
  const scrubbed = scrubValue(capture, scrubber) as T
  if (scrubbed !== null && typeof scrubbed === 'object' && 'screenshot' in scrubbed) {
    delete (scrubbed as Record<string, unknown>).screenshot
  }
  return scrubbed
}
