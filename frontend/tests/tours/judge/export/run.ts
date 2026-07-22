// CLI: ship a judge run's GenAI span records to Langfuse, enriched for triage.
//
//   QA_JUDGE_TRACE=/path/run.jsonl  JUDGE=1 make qa-report   # produce the spans
//   LANGFUSE_HOST=... LANGFUSE_PUBLIC_KEY=... LANGFUSE_SECRET_KEY=... \
//     [QA_RUN_ID=...] [QA_GIT_SHA=...] [QA_SALT_PASSES=N] \
//     bun run tests/tours/judge/export/run.ts /path/run.jsonl
//
// Opt-in by construction: with no LANGFUSE_* env this exits 0 and ships nothing,
// so a normal offline run never depends on a reachable backend.
//
// Provenance (QA_RUN_ID / QA_GIT_SHA) is stamped HERE at export time — where the
// whole trace file is read + shipped — not plumbed through the judge span builder.
// Each component is validated independently and NEVER fail-closed: an invalid one is
// dropped so the mandatory trap diagnostic still ships.

import * as fs from 'fs'
import { configFromEnv, exportSpans, parseSpanFile } from './langfuse'

// Minimal filesystem seam so run.test.ts can drive main() — including the sidecar
// guard's failure branches — without touching disk.
export interface RunFs {
  existsSync(p: string): boolean
  readFileSync(p: string, enc: 'utf8'): string
  writeFileSync(p: string, data: string): void
}

export interface RunDeps {
  fs?: RunFs
  exportSpans?: typeof exportSpans
  log?: (msg: string) => void
  errlog?: (msg: string) => void
}

const RUN_ID_RE = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/
const GIT_SHA_RE = /^[0-9a-f]{7,40}$/
const SIDECAR_SUFFIX = '.qa-provenance'

// QA_RUN_ID must match the timestamp shape AND parse to a real UTC instant that
// round-trips — a shape-only regex accepts `20269999T999999Z`.
export function validRunId(v: string | undefined): string | undefined {
  if (!v) return undefined
  const m = RUN_ID_RE.exec(v)
  if (!m) return undefined
  const [y, mo, d, h, mi, s] = [m[1], m[2], m[3], m[4], m[5], m[6]].map(Number)
  // Build via setUTC* (NOT Date.UTC), which does NOT map a 0–99 year to 1900–1999. That
  // mapping otherwise both rejects years 0000–0099 outright AND miscomputes their leap
  // days (Feb 29 of a 400-divisible year, e.g. 0000, would roll over under 1900). Out-of-
  // range fields still roll over here, so the round-trip check below rejects them.
  const dt = new Date(0)
  dt.setUTCFullYear(y, mo - 1, d)
  dt.setUTCHours(h, mi, s, 0)
  const ok =
    dt.getUTCFullYear() === y &&
    dt.getUTCMonth() === mo - 1 &&
    dt.getUTCDate() === d &&
    dt.getUTCHours() === h &&
    dt.getUTCMinutes() === mi &&
    dt.getUTCSeconds() === s
  return ok ? v : undefined
}

// A short 7-char sha is VALID and used as-is (a verified prefix); it is NOT required
// to equal any 40-char manifest sha.
export function validGitSha(v: string | undefined): string | undefined {
  return v !== undefined && GIT_SHA_RE.test(v) ? v : undefined
}

// QA_SALT_PASSES parses to a non-negative integer, else undefined (exportSpans then
// defaults to 3). 0 is honored (disable salt); empty/negative/fractional/malformed →
// drop. A strict digits-only match rejects `-1`, `2.5`, `1e2`, `0x10`, and ``.
export function parseSaltPasses(v: string | undefined): number | undefined {
  if (v === undefined) return undefined
  const t = v.trim()
  return /^\d+$/.test(t) ? Number(t) : undefined
}

// A well-formed provenance sidecar is exactly what this guard writes: an object with
// `runId` and `gitSha`, each a string or null. Anything else ({}, an array, a scalar,
// `{runId:42}`) is malformed — treated as such rather than silently coerced to null,
// which would disable stale-round detection.
function isSidecar(v: unknown): v is { runId: string | null; gitSha: string | null } {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) return false
  const o = v as Record<string, unknown>
  const strOrNull = (x: unknown): boolean => x === null || typeof x === 'string'
  return 'runId' in o && 'gitSha' in o && strOrNull(o.runId) && strOrNull(o.gitSha)
}

// Best-effort stale-round guard (contract #3). The WHOLE trace file is exported, so
// reusing a path across rounds relabels stale spans with the current provenance. If a
// sidecar records DIFFERENT provenance than this export's, warn loudly and STILL ship;
// then record the current provenance. The ENTIRE read → parse → compare → write path is
// exception-contained — any error warns that the guard degraded and returns; it never
// throws, never blocks the mandatory trap trace, never changes the export or exit code.
function staleFileGuard(
  file: string,
  runId: string | undefined,
  gitSha: string | undefined,
  rfs: RunFs,
  warn: (msg: string) => void
): void {
  const sidecar = `${file}${SIDECAR_SUFFIX}`
  const current = JSON.stringify({ runId: runId ?? null, gitSha: gitSha ?? null })
  try {
    if (rfs.existsSync(sidecar)) {
      let prior: string | undefined
      try {
        const parsed: unknown = JSON.parse(rfs.readFileSync(sidecar, 'utf8'))
        prior = isSidecar(parsed)
          ? JSON.stringify({ runId: parsed.runId, gitSha: parsed.gitSha })
          : undefined // wrong shape → cannot trust it → treat as malformed
      } catch {
        prior = undefined // unreadable/malformed sidecar → cannot compare
      }
      if (prior === undefined) {
        warn(
          `qa-export: WARNING — provenance sidecar ${sidecar} is unreadable/malformed; ` +
            'stale-round protection degraded (still shipping).'
        )
      } else if (prior !== current) {
        warn(
          `qa-export: WARNING — trace path ${file} reused across rounds; stale spans are being ` +
            'relabeled with the current provenance. Supply a fresh per-round artifact.'
        )
      }
    }
    rfs.writeFileSync(sidecar, current)
  } catch (err) {
    warn(
      `qa-export: WARNING — stale-round provenance guard could not access ${sidecar} ` +
        `(${err instanceof Error ? err.message : String(err)}); protection degraded (still shipping).`
    )
  }
}

export async function main(
  argv: string[],
  env: Record<string, string | undefined> = process.env,
  deps: RunDeps = {}
): Promise<number> {
  const rfs: RunFs = deps.fs ?? fs
  const doExport = deps.exportSpans ?? exportSpans
  const log = deps.log ?? console.log
  const errlog = deps.errlog ?? console.error

  // Opt-in guard FIRST (INV-A): with no LANGFUSE_* this exits 0 shipping nothing,
  // BEFORE any arg / trace-file validation — an offline run must never fail for a
  // missing arg it was never going to use.
  const cfg = configFromEnv(env)
  if (!cfg) {
    log(
      'qa-export: LANGFUSE_HOST / LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY not set — nothing shipped.'
    )
    return 0
  }

  const file = argv[0]
  if (!file) {
    errlog('usage: bun run tests/tours/judge/export/run.ts <trace.jsonl>')
    return 2
  }
  if (!rfs.existsSync(file)) {
    errlog(`qa-export: no trace file at ${file} — did the judge run with QA_JUDGE_TRACE set?`)
    return 1
  }

  // Component-wise provenance (contract #3): each validated independently; an invalid
  // one is dropped with a warning while the other still applies. NEVER fail-closed.
  const runId = validRunId(env.QA_RUN_ID)
  const gitSha = validGitSha(env.QA_GIT_SHA)
  // Warn whenever the var was PROVIDED but invalid (including an empty string) — only a
  // truly absent (undefined) var is silently skipped. Distinguishes "not set" from "set
  // wrong" so an operator sees the drop.
  if (env.QA_RUN_ID !== undefined && !runId)
    log(`qa-export: WARNING — QA_RUN_ID='${env.QA_RUN_ID}' is not a valid run-id — dropping it.`)
  if (env.QA_GIT_SHA !== undefined && !gitSha)
    log(`qa-export: WARNING — QA_GIT_SHA='${env.QA_GIT_SHA}' is not a valid git sha — dropping it.`)
  const saltPasses = parseSaltPasses(env.QA_SALT_PASSES)

  const spans = parseSpanFile(rfs.readFileSync(file, 'utf8'))
  if (spans.length === 0) {
    log(`qa-export: ${file} has no spans.`)
    return 0
  }

  const withContent = spans.filter(s => s.attributes['gen_ai.prompt'] !== undefined).length
  if (withContent === 0) {
    // Loud, because it is the difference between a usable label queue and an empty one.
    log(
      'qa-export: WARNING — no span carries a prompt. These traces will be UNLABELABLE ' +
        '(nothing for a reviewer to read). The judge adapter must pass `prompt` to buildGenAiSpan.'
    )
  }

  staleFileGuard(file, runId, gitSha, rfs, log)

  log(`qa-export: shipping ${spans.length} span(s) to ${cfg.host}`)
  const result = await doExport(cfg, spans, msg => log(msg), { runId, gitSha, saltPasses })
  log(
    // The ONE canonical summary line. scripts/ci/qa-nightly-round.sh parses it with a
    // FULLY ANCHORED regex requiring exactly one match, so any field added here must
    // land with the matching regex change or the nightly silently zeroes every count.
    `qa-export: ${result.traces} trace(s), ${result.screenshots} screenshot(s), ` +
      `${result.observations} observation(s)` +
      (result.failed ? `, ${result.failed} FAILED` : '') +
      `; enqueued ${result.enqueue.enqueued}/${result.enqueue.attempted}` +
      (result.enqueue.skippedExisting ? `, ${result.enqueue.skippedExisting} already queued` : '') +
      (result.enqueue.failed ? `, ${result.enqueue.failed} enqueue-failed` : '')
  )
  return result.failed > 0 ? 1 : 0
}

// Import-guarded: importing this module (as run.test.ts does) runs NO side effects;
// main() executes only when the file is the process entry point.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main(process.argv.slice(2)).then(code => {
    process.exitCode = code
  })
}
