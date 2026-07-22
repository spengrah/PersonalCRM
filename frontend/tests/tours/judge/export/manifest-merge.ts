// Merge the price-sync result into a completed round's manifest.
//
//   bun run tests/tours/judge/export/manifest-merge.ts <manifest.json> <status> [sha256]
//
// WHY A SEPARATE STEP: the price sync runs before the round's run id, run dir, and
// manifest exist, but its provenance belongs IN the manifest so a cost anomaly can
// be traced to the exact price set in force. So the orchestrator holds the result
// and merges it once tours has written the manifest.
//
// WHY ATOMIC: the caller treats a merge failure as non-fatal, and that is only true
// if a failure cannot damage the file. A direct in-place write can truncate or
// partially replace the manifest BEFORE throwing, after which the round's own
// provenance assert — which reads this same file — fails and the round is
// incomplete. Full content to a temp file in the SAME directory (rename is atomic
// only within one filesystem), then rename over the original. On ANY failure the
// original is untouched and the exit code is non-zero.
//
// It ADDS one key and never rewrites the others, which is what keeps the existing
// assert safe.

import * as fs from 'fs'
import * as path from 'path'

export interface PriceSyncProvenance {
  status: string
  upstreamSha256: string
}

/** Minimal fs seam, so the failure path — the one the atomicity exists for — can be
 * driven in a test rather than asserted about. */
export interface MergeIo {
  readFileSync: (p: string) => string
  writeFileSync: (p: string, data: string) => void
  renameSync: (from: string, to: string) => void
  unlinkSync: (p: string) => void
}

export const realIo: MergeIo = {
  readFileSync: p => fs.readFileSync(p, 'utf8'),
  writeFileSync: (p, data) => fs.writeFileSync(p, data),
  renameSync: (from, to) => fs.renameSync(from, to),
  unlinkSync: p => fs.unlinkSync(p),
}

/** PURE: the merged manifest text. Throws when the input is not a JSON object — a
 * manifest we cannot parse is one we must not overwrite. */
export function mergedManifest(raw: string, provenance: PriceSyncProvenance): string {
  const parsed: unknown = JSON.parse(raw)
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error('manifest is not a JSON object')
  }
  return `${JSON.stringify({ ...(parsed as Record<string, unknown>), priceSync: provenance }, null, 2)}\n`
}

export function mergeManifest(
  manifestPath: string,
  provenance: PriceSyncProvenance,
  io: MergeIo = realIo
): void {
  const raw = io.readFileSync(manifestPath)
  const next = mergedManifest(raw, provenance)
  const tmp = path.join(
    path.dirname(manifestPath),
    `${path.basename(manifestPath)}.tmp-${process.pid}`
  )
  try {
    io.writeFileSync(tmp, next)
    io.renameSync(tmp, manifestPath)
  } catch (err) {
    try {
      io.unlinkSync(tmp)
    } catch {
      // Nothing to clean up, or it cannot be removed — either way the original file
      // is what matters and it was never touched.
    }
    throw err
  }
}

export function main(argv: string[], errlog: (s: string) => void = s => console.error(s)): number {
  const [manifestPath, status, sha] = argv
  if (manifestPath === undefined || status === undefined) {
    errlog('manifest-merge: usage: manifest-merge.ts <manifest.json> <status> [sha256]')
    return 2
  }
  try {
    mergeManifest(manifestPath, { status, upstreamSha256: sha ?? '' })
    return 0
  } catch (err) {
    errlog(`manifest-merge: ${err instanceof Error ? err.message : String(err)}`)
    return 1
  }
}

// Import-guarded: importing this module (as the test does) runs NO side effects.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  process.exitCode = main(process.argv.slice(2))
}
