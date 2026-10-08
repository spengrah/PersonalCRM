// CLI: write a fresh synthetic judge round for the Langfuse v4 live acceptance
// captures, and print the ids and expectations each capture checks.
//
//   bun run tests/tours/judge/export/fixture-round.ts <out.jsonl>
//
// The file is an ordinary judge trace file, exported with `make qa-export
// TRACE=<out.jsonl>` under `QA_TEST_TAG`, `QA_RUN_ID=<runId>` and `QA_SALT_PASSES=0`.
// Each invocation mints new span ids and times from the clock, so every capture
// exports spans no exporter has shipped before, and `qa-cost-assert FROM=<from>`
// covers the round and little else. Exporting the same file twice re-sends the same
// ids and times.
//
// The round is two spans of catalog behavior CON-042 (the backfill rejects unknown
// behaviors), both carrying usage:
//   span A: items 0, 1, 2 graded pass, fail, pass
//   span B: item 0 graded pass
// So the export writes four traces, four verdict scores and two generations; with no
// salt it enqueues only the failing trace; and the backfill's candidates for the round
// are the three passing traces.
//
// Every string is synthetic. The printed manifest holds ids and verdicts only.

import * as fs from 'fs'
import { buildGenAiSpan, type GenAiSpan } from '../adapter/span'
import type { PerItemVerdict } from '../adapter/types'
import type { Scenario } from '../label-trace'
import { SPEC_CATALOG } from '../spec-catalog'
import { buildTraceBody, generationSpanId, hexTraceId, rootSpanId, usageTraceId } from './langfuse'

const BEHAVIOR = SPEC_CATALOG['CON-042']
const MODEL = 'gpt-5.4-mini'

function fixtureSpan(
  itemVerdicts: Array<PerItemVerdict['verdict']>,
  startMs: number,
  usage: { input: number; cached: number; output: number }
): GenAiSpan {
  const scenario: Scenario = {
    kind: 'behavior',
    behaviorId: BEHAVIOR.id,
    behaviorTitle: BEHAVIOR.title,
    given: BEHAVIOR.given,
    when: BEHAVIOR.when,
    items: itemVerdicts.map((_, itemIndex) => ({
      itemIndex,
      thenText: BEHAVIOR.then[itemIndex],
    })),
    allThen: BEHAVIOR.then,
  }
  return buildGenAiSpan({
    impl: 'fixture',
    behaviorId: BEHAVIOR.id,
    model: MODEL,
    startMs,
    endMs: startMs + 4_000,
    inputTokens: usage.input,
    cachedInputTokens: usage.cached,
    outputTokens: usage.output,
    prompt: 'Synthetic fixture prompt for the Langfuse v4 acceptance captures. No judge ran.',
    scenario,
    itemVerdicts: itemVerdicts.map((verdict, itemIndex) => ({
      itemIndex,
      verdict,
      citation: `synthetic fixture citation ${itemIndex}`,
      critique: `synthetic fixture critique ${itemIndex}`,
    })),
  })
}

// `YYYYMMDDTHHMMSSZ`, the shape `QA_RUN_ID` accepts.
function runIdAt(ms: number): string {
  return new Date(ms)
    .toISOString()
    .replace(/[-:]/g, '')
    .replace(/\.\d{3}Z$/, 'Z')
}

export interface FixtureTrace {
  labelId: string
  traceId: string
  rootSpanId: string
  verdict: PerItemVerdict['verdict']
}

// Ids and verdicts only: what each capture checks the export and the readers against.
export interface FixtureManifest {
  runId: string
  // The round's earliest span start: `qa-cost-assert FROM=<from>`.
  from: string
  behavior: string
  traces: FixtureTrace[]
  generations: Array<{ traceId: string; id: string }>
  // The backfill's candidates for this round: the passing traces.
  candidates: string[]
  // What `qa-export` enqueues with `QA_SALT_PASSES=0`: the failing traces.
  enqueued: string[]
}

export function buildFixtureRound(nowMs: number): {
  spans: GenAiSpan[]
  manifest: FixtureManifest
} {
  const startMs = Math.floor(nowMs / 1000) * 1000 - 60_000
  const spans = [
    fixtureSpan(['pass', 'fail', 'pass'], startMs, { input: 1000, cached: 400, output: 200 }),
    fixtureSpan(['pass'], startMs + 10_000, { input: 800, cached: 0, output: 150 }),
  ]
  const traces: FixtureTrace[] = spans.flatMap(span =>
    buildTraceBody(span).map(body => ({
      labelId: body.id,
      traceId: hexTraceId(body.id),
      rootSpanId: rootSpanId(body.id),
      verdict: (body.output as PerItemVerdict).verdict,
    }))
  )
  const manifest: FixtureManifest = {
    runId: runIdAt(nowMs),
    from: new Date(startMs).toISOString(),
    behavior: BEHAVIOR.id,
    traces,
    generations: spans.map(span => ({
      traceId: hexTraceId(usageTraceId(span)),
      id: generationSpanId(span),
    })),
    candidates: traces.filter(t => t.verdict === 'pass').map(t => t.traceId),
    enqueued: traces.filter(t => t.verdict === 'fail').map(t => t.traceId),
  }
  return { spans, manifest }
}

function main(argv: string[]): number {
  const out = argv[0]
  if (!out) {
    console.error('usage: bun run tests/tours/judge/export/fixture-round.ts <out.jsonl>')
    return 2
  }
  const { spans, manifest } = buildFixtureRound(Date.now())
  fs.writeFileSync(out, spans.map(s => JSON.stringify(s)).join('\n') + '\n')
  console.log(JSON.stringify({ file: out, ...manifest }, null, 2))
  return 0
}

// Import-guarded: main() runs only when the file is the process entry point.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  process.exitCode = main(process.argv.slice(2))
}
