// CLI: ship a judge run's GenAI span records to Langfuse.
//
//   QA_JUDGE_TRACE=/path/run.jsonl  JUDGE=1 make qa-report   # produce the spans
//   LANGFUSE_HOST=... LANGFUSE_PUBLIC_KEY=... LANGFUSE_SECRET_KEY=... \
//     bun run tests/tours/judge/export/run.ts /path/run.jsonl
//
// Opt-in by construction: with no LANGFUSE_* env this exits 0 and ships nothing,
// so a normal offline run never depends on a reachable backend.

import * as fs from 'fs'
import { configFromEnv, exportSpans, parseSpanFile } from './langfuse'

const file = process.argv[2]
if (!file) {
  console.error('usage: bun run tests/tours/judge/export/run.ts <trace.jsonl>')
  process.exit(2)
}
if (!fs.existsSync(file)) {
  console.error(`qa-export: no trace file at ${file} — did the judge run with QA_JUDGE_TRACE set?`)
  process.exit(1)
}

const cfg = configFromEnv()
if (!cfg) {
  console.log(
    'qa-export: LANGFUSE_HOST / LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY not set — nothing shipped.'
  )
  process.exit(0)
}

const spans = parseSpanFile(fs.readFileSync(file, 'utf8'))
if (spans.length === 0) {
  console.log(`qa-export: ${file} has no spans.`)
  process.exit(0)
}

const withContent = spans.filter(s => s.attributes['gen_ai.prompt'] !== undefined).length
if (withContent === 0) {
  // Loud, because it is the difference between a usable label queue and an empty one.
  console.warn(
    'qa-export: WARNING — no span carries a prompt. These traces will be UNLABELABLE ' +
      '(nothing for a reviewer to read). The judge adapter must pass `prompt` to buildGenAiSpan.'
  )
}

console.log(`qa-export: shipping ${spans.length} span(s) to ${cfg.host}`)
const result = await exportSpans(cfg, spans, msg => console.log(msg))
console.log(
  `qa-export: ${result.traces} trace(s), ${result.screenshots} screenshot(s)` +
    (result.failed ? `, ${result.failed} FAILED` : '')
)
process.exit(result.failed > 0 ? 1 : 0)
