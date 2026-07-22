// Upstream price-table fixtures.
//
// `upstream-model-prices.sample.json` holds VERBATIM excerpts of upstream's
// `default-model-prices.json` — the four models a round can send plus one entry
// that carries no tokenizer. Verbatim is the point: the selection tests must run
// against real `(?i)`-prefixed patterns (valid in Postgres, a SyntaxError in
// JavaScript), and a hand-written pattern would let that trap survive review.
// Server-owned fields (`id`, `createdAt`, `updatedAt`, per-tier `id`) are kept in
// the file exactly as upstream ships them, so the parser is exercised against the
// real payload's extra keys rather than a tidied version of it.
//
// TWO sets, because one cannot serve both selection tests: the unique set has
// exactly one entry matching each target (unique selection, tier fidelity,
// reconciliation), and the ambiguous set adds a decoy that ALSO matches one target
// (ambiguity refusal). Under a single decoy-bearing set the unique-selection test
// would have to expect a throw — the two assertions are mutually exclusive.

import * as fs from 'fs'
import * as path from 'path'
import { fileURLToPath } from 'url'

const HERE = path.dirname(fileURLToPath(import.meta.url))

/** The fixture's RAW bytes — what a provenance hash must cover. */
export const UNIQUE_RAW: string = fs.readFileSync(
  path.join(HERE, 'upstream-model-prices.sample.json'),
  'utf8'
)

export const UNIQUE_SET: Record<string, unknown>[] = JSON.parse(UNIQUE_RAW) as Record<
  string,
  unknown
>[]

/** A decoy whose pattern ALSO matches `gpt-5.5`. Necessarily fabricated: upstream
 * maintains one entry per model, so no real pair double-matches — which is exactly
 * why the ambiguity path can never be reached by accident and has to be staged. */
export const DECOY: Record<string, unknown> = {
  modelName: 'gpt-5.5-preview',
  matchPattern: '(?i)^(openai\\/)?(gpt-5\\.5.*)$',
  pricingTiers: [
    {
      name: 'Standard',
      isDefault: true,
      priority: 0,
      conditions: [],
      prices: { input: 1e-6, output: 4e-6 },
    },
  ],
}

export const AMBIGUOUS_SET: Record<string, unknown>[] = [...UNIQUE_SET, DECOY]

/** The verbatim upstream entry for one model name in the sample. */
export function sampleEntry(modelName: string): Record<string, unknown> {
  const hit = UNIQUE_SET.find(m => m.modelName === modelName)
  if (hit === undefined) throw new Error(`fixture has no entry named ${modelName}`)
  return hit
}
