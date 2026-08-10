// The checked-in price file's schema: row types, load/validate, matchPattern
// emission, collision detection, and deterministic serialization.
//
// This module owns the ONLY place matchPattern strings are constructed. They are
// emitted mechanically from a row's declared fields, never authored by hand — see
// emitMatchPattern. `(?i)` is a Postgres-flavored inline-flag prefix (valid in
// Langfuse's matchPattern column, a SyntaxError in JavaScript), so compileMatchPattern
// strips it before handing the remainder to the JS RegExp engine. That is safe here,
// and only here, because every pattern this file produces comes from the two
// templates below.

export type Source = 'venice' | 'open-router'

export interface ModelPrices {
  input: number
  output: number
  cachedInput?: number
}

export interface ModelRow {
  // The model string the usage stream reports to Langfuse (the judge's Codex path
  // reports bare OpenAI names like "gpt-5.6-luna"; a Venice call reports Venice's
  // own model id). This is also the string matchPattern is checked against, and the
  // identity collisions are validated over.
  modelName: string
  // Price provenance, not model kind — see the spec's Goals section.
  source: Source
  // The source-side catalog lookup key. For open-router this differs from
  // modelName (e.g. "openai/gpt-5.6-luna"); for venice rows it is conventionally
  // the same string as modelName, since Venice's own id IS what the usage stream
  // reports.
  sourceId: string
  prices: ModelPrices
}

export interface ModelPricesFile {
  models: ModelRow[]
}

const SOURCES: Source[] = ['venice', 'open-router']

function isFiniteNumber(v: unknown): v is number {
  return typeof v === 'number' && Number.isFinite(v)
}

function parsePrices(raw: unknown, where: string): ModelPrices {
  if (raw === null || typeof raw !== 'object') {
    throw new Error(`${where}: prices is not an object`)
  }
  const r = raw as Record<string, unknown>
  if (!isFiniteNumber(r.input)) {
    throw new Error(`${where}: prices.input is not a finite number`)
  }
  if (!isFiniteNumber(r.output)) {
    throw new Error(`${where}: prices.output is not a finite number`)
  }
  if (r.cachedInput !== undefined && !isFiniteNumber(r.cachedInput)) {
    throw new Error(`${where}: prices.cachedInput is present but not a finite number`)
  }
  return {
    input: r.input,
    output: r.output,
    ...(r.cachedInput !== undefined ? { cachedInput: r.cachedInput as number } : {}),
  }
}

function parseRow(raw: unknown, index: number): ModelRow {
  const where = `model-prices.json: models[${index}]`
  if (raw === null || typeof raw !== 'object') {
    throw new Error(`${where} is not an object`)
  }
  const r = raw as Record<string, unknown>
  if (typeof r.modelName !== 'string' || r.modelName.length === 0) {
    throw new Error(`${where}: modelName is not a non-empty string`)
  }
  if (typeof r.source !== 'string' || !SOURCES.includes(r.source as Source)) {
    throw new Error(`${where} (${r.modelName}): source must be one of ${SOURCES.join(', ')}, got ${JSON.stringify(r.source)}`)
  }
  if (typeof r.sourceId !== 'string' || r.sourceId.length === 0) {
    throw new Error(`${where} (${r.modelName}): sourceId is not a non-empty string`)
  }
  const prices = parsePrices(r.prices, `${where} (${r.modelName})`)
  return { modelName: r.modelName, source: r.source as Source, sourceId: r.sourceId, prices }
}

// Parse an already-JSON.parse'd value into a typed, validated file. Throws loudly
// on any structural problem, including collisions (see checkCollisions) — a file
// that fails to parse must never be treated as "no rows".
export function parseFile(raw: unknown): ModelPricesFile {
  if (raw === null || typeof raw !== 'object') {
    throw new Error('model-prices.json: root is not an object')
  }
  const r = raw as Record<string, unknown>
  if (!Array.isArray(r.models)) {
    throw new Error('model-prices.json: models is not an array')
  }
  const models = r.models.map((m, i) => parseRow(m, i))
  checkCollisions(models)
  return { models }
}

// Escape every character emitMatchPattern's templates treat as literal, so the
// derived regex matches the source string exactly and nothing broader. Escaping
// `-` and `/` alongside the standard regex metacharacters is deliberate belt-and-
// suspenders: neither is special outside a character class, but escaping them
// keeps the emitted pattern visually unambiguous on review and matches what the
// contract tests pin.
function escapeForPattern(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\/-]/g, '\\$&')
}

// Mechanical matchPattern emission — the ONLY place a matchPattern is built. Venice
// rows get the optional "venice/" prefix Venice's own naming sometimes carries;
// every other source gets an exact-match pattern on its model string.
export function emitMatchPattern(row: ModelRow): string {
  if (row.source === 'venice') {
    return `(?i)^(venice\\/)?${escapeForPattern(row.sourceId)}$`
  }
  return `(?i)^${escapeForPattern(row.modelName)}$`
}

// Strip the Postgres-flavored `(?i)` prefix and compile the remainder as a JS
// RegExp with the `i` flag. Safe ONLY because every pattern handed to this
// function was produced by emitMatchPattern above — this file never evaluates a
// matchPattern it did not itself emit.
export function compileMatchPattern(pattern: string): RegExp {
  if (!pattern.startsWith('(?i)')) {
    throw new Error(`matchPattern does not start with (?i): ${pattern}`)
  }
  return new RegExp(pattern.slice('(?i)'.length), 'i')
}

// Collisions make cost attribution ambiguous by construction: if two rows' patterns
// both match one usage-stream model string, Langfuse's own resolution order decides
// which one prices it, silently. Checked against every row's OWN modelName as the
// candidate target set — that is what Langfuse actually compares matchPattern
// against.
export function checkCollisions(rows: ModelRow[]): void {
  // Self-match first: a row whose emitted pattern does not match its own
  // modelName prices nothing — its usage stream would resolve to no definition
  // (e.g. a venice row whose sourceId diverges from the model string it reports).
  for (const row of rows) {
    if (!compileMatchPattern(emitMatchPattern(row)).test(row.modelName)) {
      throw new Error(
        `model-prices.json: row ${row.modelName} (source ${row.source}, sourceId ${row.sourceId}) emits a matchPattern that does not match its own modelName`
      )
    }
  }
  for (const target of rows.map(r => r.modelName)) {
    const matches = rows.filter(r => compileMatchPattern(emitMatchPattern(r)).test(target))
    if (matches.length > 1) {
      throw new Error(
        `model-prices.json: rows ${matches.map(m => m.modelName).join(', ')} all match "${target}" — matchPattern collision`
      )
    }
  }
}

// Deterministic output: stable sort by modelName, stable key order, 2-space
// indent, trailing newline — so a no-drift sync produces byte-identical bytes and
// every diff a human reviews is exactly the price change, nothing else.
export function serialize(file: ModelPricesFile): string {
  const sorted = [...file.models].sort((a, b) => a.modelName.localeCompare(b.modelName))
  const canonical = {
    models: sorted.map(r => ({
      modelName: r.modelName,
      source: r.source,
      sourceId: r.sourceId,
      prices: {
        input: r.prices.input,
        output: r.prices.output,
        ...(r.prices.cachedInput !== undefined ? { cachedInput: r.prices.cachedInput } : {}),
      },
    })),
  }
  return JSON.stringify(canonical, null, 2) + '\n'
}
