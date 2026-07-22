// CLI: qa-model-prices — keep this project's Langfuse model-price definitions in
// step with upstream's price table, for the models a QA round actually sends.
//
// WHY THIS EXISTS: Langfuse infers cost from the model string plus a price
// definition. The instance's definitions are Langfuse-MANAGED rows baked into the
// worker image and applied on worker start — there is no runtime fetch, so between
// image upgrades the instance's prices drift from upstream silently, and a judge
// bakeoff decided on stale prices is decided on fiction.
//
// WHY IT RECONCILES RATHER THAN WRITES: a project-scoped definition is a
// PRECEDENCE claim, not a value claim. The server resolves
// `ORDER BY project_id ASC, start_date DESC NULLS LAST LIMIT 1`, so ANY
// project-scoped row outranks the managed one permanently — even when the prices
// are byte-identical. A sync that only ever ADDS overrides therefore appoints
// itself sole price maintainer for every model it touches, forever. Instead each
// run drives a target to one of five states:
//
//   managed matches upstream, no override        -> nothing (the common case)
//   managed matches upstream, override present   -> DELETE the override (hand back)
//   managed stale/absent, no override            -> POST an override
//   managed stale, override differs from upstream-> DELETE + POST
//   managed stale, override matches upstream     -> nothing
//
// Row 2 closes the door by itself: once an image upgrade brings the managed row
// current, the next run removes our override with no human involved.
//
// WHY NOT WRITE THE MANAGED ROWS: there is no API for it — the public route always
// writes project-scoped rows — so it would need direct Postgres access and writes
// to a table owned by another application's migrations. The public API is a
// contract; the schema is not.
//
// RESOLUTION HAPPENS AT INGESTION TIME, not observation time: the server never
// receives the observation's start time, so a definition applies to everything
// ingested after it is written. That is why this must run before the EXPORT (not
// merely before the judge), and why re-exporting history reprices it at today's
// prices rather than replaying the prices that were in force.

import * as crypto from 'crypto'
import * as fs from 'fs'
import { api, apiGetAllPages, configFromEnv } from './langfuse'
import type { LangfuseConfig } from './langfuse'
import { activeModels } from '../models'

// Upstream's price table. Deliberately unpinned — currency is the whole point —
// which is exactly why every guard below exists.
export const UPSTREAM_URL =
  'https://raw.githubusercontent.com/langfuse/langfuse/main/worker/src/constants/default-model-prices.json'

// The Langfuse version whose model-price RESOLUTION behavior this script was read
// and verified against. The deployed server's ordering contradicts the published
// OpenAPI (which describes a date-vs-observation comparison the code does not do),
// so the ordering this file mirrors is a property of the deployed code, not of the
// documented contract. A different version is a signal to re-verify, not an error.
export const VERIFIED_LANGFUSE_VERSION = '3.212.0'

// Every outbound call is bounded: a hung fetch would otherwise stall the whole
// nightly round, and a round's fail-open only means anything if the step ends.
const UPSTREAM_TIMEOUT_MS = 30_000
const API_TIMEOUT_MS = 30_000
const HEALTH_TIMEOUT_MS = 10_000

// A price moving by MORE than this factor in either direction is refused without
// --force. The boundary is exclusive on purpose: a real 5x cut has happened, and a
// legitimate change of that size should not need forcing.
const MAX_PLAUSIBLE_RATIO = 5
// Floating-point division makes an EXACTLY 5x move compute as 5.000000000000001, so
// a bare `>` would refuse the very case the exclusive boundary exists to allow.
const RATIO_EPSILON = 1e-9

// The create schema wants a unit; managed rows carry null. Sent on every create and
// NEVER compared, or every diff would report a phantom delta forever.
const CREATE_UNIT = 'TOKENS'

export const EXIT = { OK: 0, FAIL: 1, USAGE: 2 } as const

// ---------------------------------------------------------------------------
// Errors — each one names a distinct refusal so a caller can report it precisely.
// ---------------------------------------------------------------------------

/** The fetched payload is not the shape this sync mirrors. Rejects the WHOLE run. */
export class UpstreamShapeError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'UpstreamShapeError'
  }
}

/** A 2xx model row from the instance violates its own schema. Fails CLOSED: a row
 * silently dropped to "absent" would make an existing override look gone (and get
 * re-created) or a managed row look missing (and suppress a hand-back). */
export class InstanceShapeError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'InstanceShapeError'
  }
}

/** The target matches more than one upstream entry. Langfuse's own tiebreak is not
 * ours to assume, so this is refused rather than guessed. */
export class AmbiguousMatchError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'AmbiguousMatchError'
  }
}

/** The target matches ZERO upstream entries — renamed or removed upstream. Reported
 * loudly with the existing definition untouched; never read as "price is now zero". */
export class NotFoundUpstreamError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'NotFoundUpstreamError'
  }
}

/** Rows tie on startDate and DIFFER, so which one the server resolves is unknowable
 * from here. Refused rather than repaired — see effectiveWithin. */
export class AmbiguousTieError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'AmbiguousTieError'
  }
}

/** A match pattern carries an inline flag this port does not understand. Thrown
 * rather than swallowed: a caught SyntaxError selects nothing and reports success,
 * which is the silent no-op this whole selection path exists to prevent. */
export class PatternError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'PatternError'
  }
}

/** The upstream payload could not be fetched (transport error, non-2xx, timeout). */
export class UpstreamFetchError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'UpstreamFetchError'
  }
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface UpstreamTier {
  name?: string
  isDefault: boolean
  priority: number
  conditions: unknown[]
  prices: Record<string, number>
}

export interface UpstreamModel {
  modelName: string
  matchPattern: string
  pricingTiers: UpstreamTier[]
  tokenizerId?: string
  tokenizerConfig?: unknown
}

/** One model definition as the instance reports it.
 *
 * The public read exposes `isLangfuseManaged`, NOT `project_id` — the column the
 * server orders by. They are the same fact: a managed row is exactly a row with
 * `project_id IS NULL`. Partitioning on the reported boolean is what keeps this
 * script off the schema. */
export interface InstanceModel {
  id: string
  modelName: string
  matchPattern: string
  isLangfuseManaged: boolean
  startDate: string | null
  startDateMs: number | null
  pricingTiers: UpstreamTier[]
}

/** A definition's price-bearing surface — the only thing diffPrices compares. */
export interface PriceCarrier {
  matchPattern: string
  pricingTiers: UpstreamTier[]
}

export interface PriceDelta {
  /** Tier key: the tier name when it has one, else its priority. */
  tier: string
  /** Set on a price-map delta; absent on a structural one. */
  usageType?: string
  from?: number
  to?: number
  detail: string
}

export interface GuardVerdict {
  ok: boolean
  reason?: string
}

export type ActionKind = 'none' | 'create' | 'replace' | 'delete'

export interface Action {
  kind: ActionKind
  reason: string
}

export interface CreateModelBody {
  modelName: string
  matchPattern: string
  unit: string
  pricingTiers: UpstreamTier[]
  tokenizerId?: string
  tokenizerConfig?: unknown
}

// ---------------------------------------------------------------------------
// Upstream parsing — strict, because this is untrusted input from an unpinned URL
// that determines every cost number downstream.
// ---------------------------------------------------------------------------

const isPlainObject = (v: unknown): v is Record<string, unknown> =>
  v !== null && typeof v === 'object' && !Array.isArray(v)

const nonEmptyString = (v: unknown): v is string => typeof v === 'string' && v.length > 0

function parseTier(raw: unknown, where: string): UpstreamTier {
  if (!isPlainObject(raw)) throw new UpstreamShapeError(`${where}: tier is not an object`)
  if (typeof raw.isDefault !== 'boolean') {
    throw new UpstreamShapeError(`${where}: tier isDefault is not a boolean`)
  }
  if (typeof raw.priority !== 'number' || !Number.isFinite(raw.priority)) {
    throw new UpstreamShapeError(`${where}: tier priority is not a finite number`)
  }
  if (!Array.isArray(raw.conditions)) {
    throw new UpstreamShapeError(`${where}: tier conditions is not an array`)
  }
  if (raw.name !== undefined && typeof raw.name !== 'string') {
    throw new UpstreamShapeError(`${where}: tier name is present but not a string`)
  }
  if (!isPlainObject(raw.prices)) {
    throw new UpstreamShapeError(`${where}: tier prices is not an object`)
  }
  const priceKeys = Object.keys(raw.prices)
  // An EMPTY price map is the malformed shape that would otherwise sail straight
  // through the diff and be written as a definition pricing everything at zero —
  // "the price is now nothing", arriving through the front door.
  if (priceKeys.length === 0) throw new UpstreamShapeError(`${where}: tier prices is empty`)
  const prices: Record<string, number> = {}
  for (const k of priceKeys) {
    const v = (raw.prices as Record<string, unknown>)[k]
    if (typeof v !== 'number' || !Number.isFinite(v)) {
      throw new UpstreamShapeError(`${where}: price '${k}' is not a finite number`)
    }
    prices[k] = v
  }
  return {
    ...(typeof raw.name === 'string' ? { name: raw.name } : {}),
    isDefault: raw.isDefault,
    priority: raw.priority,
    conditions: raw.conditions,
    prices,
  }
}

/** Parse + validate the upstream payload. Throws on ANY unexpected structure and
 * never returns a partial list: a payload that is wrong anywhere is a payload we
 * cannot reason about anywhere.
 *
 * Tiers are normalized to their price-bearing fields. Upstream's server-owned tier
 * `id`s are deliberately NOT carried into a create body — they identify rows in
 * another deployment's table, not prices. Everything that decides a cost —
 * conditions, priority, isDefault, name, the price maps — is mirrored as-is. */
export function parseUpstream(raw: string): UpstreamModel[] {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch (err) {
    // A truncated body or an HTML error page lands here.
    throw new UpstreamShapeError(
      `payload is not JSON: ${err instanceof Error ? err.message : String(err)}`
    )
  }
  if (!Array.isArray(parsed)) throw new UpstreamShapeError('payload root is not an array')
  if (parsed.length === 0) throw new UpstreamShapeError('payload root is an empty array')

  return parsed.map((rawModel, i) => {
    const where = `entry ${i}`
    if (!isPlainObject(rawModel)) throw new UpstreamShapeError(`${where}: not an object`)
    if (!nonEmptyString(rawModel.modelName)) {
      throw new UpstreamShapeError(`${where}: modelName is not a non-empty string`)
    }
    if (!nonEmptyString(rawModel.matchPattern)) {
      throw new UpstreamShapeError(`${where}: matchPattern is not a non-empty string`)
    }
    if (!Array.isArray(rawModel.pricingTiers) || rawModel.pricingTiers.length === 0) {
      throw new UpstreamShapeError(`${where}: pricingTiers is not a non-empty array`)
    }
    if (rawModel.tokenizerId !== undefined && typeof rawModel.tokenizerId !== 'string') {
      // Forwarded verbatim into the create body, so an unvalidated non-string would
      // be written straight into a definition.
      throw new UpstreamShapeError(`${where}: tokenizerId is present but not a string`)
    }
    const tiers = rawModel.pricingTiers.map((t, n) => parseTier(t, `${where} tier ${n}`))
    const defaults = tiers.filter(t => t.isDefault)
    if (defaults.length !== 1) {
      throw new UpstreamShapeError(
        `${where}: expected exactly one default tier, found ${defaults.length}`
      )
    }
    // A default tier is the unconditional floor. A conditional or non-zero-priority
    // "default" is a different schema than the one this sync mirrors.
    if (defaults[0].priority !== 0) {
      throw new UpstreamShapeError(
        `${where}: default tier priority is ${defaults[0].priority}, expected 0`
      )
    }
    if (defaults[0].conditions.length !== 0) {
      throw new UpstreamShapeError(`${where}: default tier carries conditions`)
    }
    return {
      modelName: rawModel.modelName,
      matchPattern: rawModel.matchPattern,
      pricingTiers: tiers,
      ...(typeof rawModel.tokenizerId === 'string' ? { tokenizerId: rawModel.tokenizerId } : {}),
      ...(rawModel.tokenizerConfig !== undefined
        ? { tokenizerConfig: rawModel.tokenizerConfig }
        : {}),
    }
  })
}

/** Parse one instance row. Fails CLOSED on a malformed row (see InstanceShapeError). */
export function parseInstanceRow(raw: unknown, where: string): InstanceModel {
  if (!isPlainObject(raw)) throw new InstanceShapeError(`${where}: row is not an object`)
  if (!nonEmptyString(raw.id))
    throw new InstanceShapeError(`${where}: id is not a non-empty string`)
  if (!nonEmptyString(raw.modelName)) {
    throw new InstanceShapeError(`${where}: modelName is not a non-empty string`)
  }
  if (!nonEmptyString(raw.matchPattern)) {
    throw new InstanceShapeError(`${where}: matchPattern is not a non-empty string`)
  }
  // The managed/override partition decides whether a row may be deleted at all, so
  // a missing or non-boolean flag is unusable rather than assumable.
  if (typeof raw.isLangfuseManaged !== 'boolean') {
    throw new InstanceShapeError(`${where}: isLangfuseManaged is not a boolean`)
  }
  let startDate: string | null = null
  let startDateMs: number | null = null
  if (raw.startDate !== null && raw.startDate !== undefined) {
    if (typeof raw.startDate !== 'string') {
      throw new InstanceShapeError(`${where}: startDate is neither a string nor null`)
    }
    const ms = Date.parse(raw.startDate)
    if (Number.isNaN(ms)) throw new InstanceShapeError(`${where}: startDate is unparseable`)
    startDate = raw.startDate
    startDateMs = ms
  }
  // A legacy flat-priced row reports an EMPTY tier array; that is a real state, not
  // a malformed one — it simply differs from every tiered upstream entry.
  const rawTiers =
    raw.pricingTiers === undefined || raw.pricingTiers === null ? [] : raw.pricingTiers
  if (!Array.isArray(rawTiers))
    throw new InstanceShapeError(`${where}: pricingTiers is not an array`)
  const pricingTiers = rawTiers.map((t, n) => {
    try {
      return parseTier(t, `${where} tier ${n}`)
    } catch (err) {
      throw new InstanceShapeError(err instanceof Error ? err.message : String(err))
    }
  })
  return {
    id: raw.id,
    modelName: raw.modelName,
    matchPattern: raw.matchPattern,
    isLangfuseManaged: raw.isLangfuseManaged,
    startDate,
    startDateMs,
    pricingTiers,
  }
}

// ---------------------------------------------------------------------------
// Pattern compilation + selection
// ---------------------------------------------------------------------------

/** Compile a Langfuse match pattern into a JS RegExp.
 *
 * Every relevant upstream pattern begins with the inline flag `(?i)`, which is
 * valid in Postgres and a SyntaxError in JavaScript. Strip it and apply the `i`
 * flag. Any OTHER inline-flag prefix throws instead of being guessed at, and a
 * SyntaxError from the body is never caught-and-continued: a swallowed one selects
 * nothing and reports success. */
export function toJsRegex(pattern: string): RegExp {
  const inline = /^\(\?([a-zA-Z]+)\)/.exec(pattern)
  let flags = ''
  let body = pattern
  if (inline !== null) {
    if (inline[1] !== 'i') {
      throw new PatternError(
        `unsupported inline regex flag '(?${inline[1]})' in pattern ${pattern}`
      )
    }
    flags = 'i'
    body = pattern.slice(inline[0].length)
  }
  try {
    return new RegExp(body, flags)
  } catch (err) {
    throw new PatternError(
      `pattern ${pattern} does not compile: ${err instanceof Error ? err.message : String(err)}`
    )
  }
}

/** The one upstream entry whose match pattern matches the target string.
 *
 * Selection is by PATTERN, never by modelName equality: the intent pass's model
 * string `gpt-5.5` is served by an entry named `gpt-5.5-2026-04-23`, so equality
 * finds nothing and the sync silently no-ops on it. */
export function selectUpstream(models: UpstreamModel[], target: string): UpstreamModel {
  const hits = models.filter(m => toJsRegex(m.matchPattern).test(target))
  if (hits.length === 0) {
    throw new NotFoundUpstreamError(`no upstream entry matches '${target}'`)
  }
  if (hits.length > 1) {
    throw new AmbiguousMatchError(
      `${hits.length} upstream entries match '${target}': ${hits.map(h => h.modelName).join(', ')}`
    )
  }
  return hits[0]
}

/** Every instance row matching the target, partitioned into managed and overrides. */
export function splitInstanceRows(
  rows: InstanceModel[],
  target: string
): { managed: InstanceModel[]; overrides: InstanceModel[] } {
  const matching = rows.filter(r => toJsRegex(r.matchPattern).test(target))
  return {
    managed: matching.filter(r => r.isLangfuseManaged),
    overrides: matching.filter(r => !r.isLangfuseManaged),
  }
}

/** Project-scoped rows that carry `modelName` but whose pattern does NOT match the
 * target.
 *
 * Uniqueness on the create route is `(project, modelName)` — the PATTERN is not part
 * of the key. So when upstream changes or widens a model's regex while keeping its
 * name, an override written under the OLD pattern stops matching the target, drops
 * out of a pattern-only search, and silently 400s every subsequent create. The model
 * can then never converge without a human deleting the row by hand. Finding these by
 * name is what keeps a routine upstream regex change from being a permanent deadlock.
 *
 * They are found for DELETION only: a row whose pattern does not match the target
 * does not serve the target, so it must not influence which action is chosen or what
 * the guard measures. */
export function blockingOverrides(
  rows: InstanceModel[],
  target: string,
  modelName: string
): InstanceModel[] {
  return rows.filter(
    r =>
      !r.isLangfuseManaged && r.modelName === modelName && !toJsRegex(r.matchPattern).test(target)
  )
}

/** The row the server would resolve WITHIN one partition: newest startDate, NULLS
 * LAST — mirroring `ORDER BY start_date DESC NULLS LAST` inside the project_id
 * group. NO further tiebreak, because the server has none: its ordering ENDS
 * there, so among tied rows the server's pick is indeterminate.
 *
 * Tied top rows that are mutually identical resolve to any one of them — every
 * possible server pick is the same definition, so any is correct. Tied top rows
 * that DIFFER throw AmbiguousTieError: the effective definition is unknowable from
 * here, and an invented tiebreak would let this script act on a row the server does
 * not serve. Ties are realistic rather than theoretical — every override this sync
 * creates omits startDate, so two of them tie on NULL immediately. */
export function effectiveWithin(rows: InstanceModel[]): InstanceModel | undefined {
  if (rows.length === 0) return undefined
  const dated = rows.filter(r => r.startDateMs !== null)
  let top: InstanceModel[]
  if (dated.length > 0) {
    const newest = Math.max(...dated.map(r => r.startDateMs as number))
    top = dated.filter(r => r.startDateMs === newest)
  } else {
    top = rows
  }
  if (top.length === 1) return top[0]
  const first = top[0]
  for (const other of top.slice(1)) {
    if (diffPrices(first, other).length > 0) {
      throw new AmbiguousTieError(`${top.length} rows tied on startDate with differing prices`)
    }
  }
  return first
}

/** The definition currently IN FORCE for the target — the server's full ordering
 * across both partitions (custom before managed, then newest startDate NULLS LAST).
 *
 * REPORTING and guard-baseline use only. It must never decide the action: its
 * custom-first bias is precisely what HIDES whether the managed row underneath has
 * caught up with upstream, which is the signal the hand-back state depends on. */
export function selectEffective(rows: InstanceModel[], target: string): InstanceModel | undefined {
  const { managed, overrides } = splitInstanceRows(rows, target)
  return effectiveWithin(overrides) ?? effectiveWithin(managed)
}

// ---------------------------------------------------------------------------
// Diff
// ---------------------------------------------------------------------------

/** Order-insensitive over object KEYS, order-SENSITIVE over arrays, numbers by
 * value. Key insensitivity matters: a naive JSON.stringify comparison would read
 * API key reordering as drift and replace the override every single night. Array
 * order is kept significant because a conditions array is an ordered predicate list
 * whose order can be meaningful, and sorting it would be a guess about a schema we
 * do not own. */
function canonicalEqual(a: unknown, b: unknown): boolean {
  if (Array.isArray(a) || Array.isArray(b)) {
    if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false
    return a.every((v, i) => canonicalEqual(v, b[i]))
  }
  if (isPlainObject(a) && isPlainObject(b)) {
    const ak = Object.keys(a).sort()
    const bk = Object.keys(b).sort()
    if (ak.length !== bk.length) return false
    if (ak.some((k, i) => k !== bk[i])) return false
    return ak.every(k => canonicalEqual(a[k], b[k]))
  }
  return a === b
}

const tierKey = (t: UpstreamTier): string =>
  typeof t.name === 'string' ? `name:${t.name}` : `priority:${t.priority}`

/** Compare two definitions' price-bearing surface. `[]` means identical.
 *
 * `from` is the definition in force, `to` is upstream. `unit` is NEVER compared
 * (upstream has none and the create schema wants one — comparing it would report a
 * phantom delta forever), and neither is the response's deprecated top-level
 * `prices`, whose shape differs from the tier price maps. */
export function diffPrices(from: PriceCarrier, to: PriceCarrier): PriceDelta[] {
  const deltas: PriceDelta[] = []
  if (from.matchPattern !== to.matchPattern) {
    // A stale pattern is drift that matters on its own: the judge's model string
    // may stop resolving to this definition at all.
    deltas.push({
      tier: '-',
      detail: `matchPattern ${from.matchPattern} -> ${to.matchPattern}`,
    })
  }
  const fromTiers = new Map(from.pricingTiers.map(t => [tierKey(t), t]))
  const toTiers = new Map(to.pricingTiers.map(t => [tierKey(t), t]))
  const keys = [...new Set([...fromTiers.keys(), ...toTiers.keys()])]
  for (const key of keys) {
    const f = fromTiers.get(key)
    const t = toTiers.get(key)
    if (f === undefined || t === undefined) {
      deltas.push({ tier: key, detail: f === undefined ? 'tier added' : 'tier removed' })
      // Per-usage-type deltas as well, so a tier that vanishes surfaces its prices
      // as "previously present, now absent" to the guard rather than slipping
      // through as a structural note.
      const present = (f ?? t) as UpstreamTier
      for (const [usageType, price] of Object.entries(present.prices)) {
        deltas.push(
          f === undefined
            ? { tier: key, usageType, to: price, detail: `${usageType} added` }
            : { tier: key, usageType, from: price, detail: `${usageType} removed` }
        )
      }
      continue
    }
    if (f.isDefault !== t.isDefault) {
      deltas.push({ tier: key, detail: `isDefault ${f.isDefault} -> ${t.isDefault}` })
    }
    if (f.priority !== t.priority) {
      deltas.push({ tier: key, detail: `priority ${f.priority} -> ${t.priority}` })
    }
    if (f.name !== t.name) {
      deltas.push({ tier: key, detail: `name ${String(f.name)} -> ${String(t.name)}` })
    }
    if (!canonicalEqual(f.conditions, t.conditions)) {
      deltas.push({ tier: key, detail: 'conditions differ' })
    }
    const usageTypes = [...new Set([...Object.keys(f.prices), ...Object.keys(t.prices)])]
    for (const usageType of usageTypes) {
      const a = f.prices[usageType]
      const b = t.prices[usageType]
      if (a === b) continue
      deltas.push({
        tier: key,
        usageType,
        ...(a !== undefined ? { from: a } : {}),
        ...(b !== undefined ? { to: b } : {}),
        detail: `${usageType} ${fmtPrice(a)} -> ${fmtPrice(b)}`,
      })
    }
  }
  return deltas
}

const fmtPrice = (n: number | undefined): string => {
  if (n === undefined) return 'absent'
  if (n === 0) return '0'
  return Number.isInteger(n) && Math.abs(n) < 1e6 ? String(n) : n.toExponential()
}

// ---------------------------------------------------------------------------
// Guard
// ---------------------------------------------------------------------------

/** Refuse implausible price movement. Billing-relevant config fed from an unpinned
 * public URL: a real change of this size is rare enough to be worth a human look,
 * and a corrupted one is exactly this shape. Symmetric in both directions. A price
 * that arrives zero, negative, or absent where one previously existed is refused
 * outright — "the price is now nothing" is never accepted silently. */
export function guardDelta(deltas: PriceDelta[]): GuardVerdict {
  for (const d of deltas) {
    if (d.usageType === undefined) continue
    const { from, to } = d
    // A newly-priced usage type has no prior value to be implausible against.
    if (from === undefined) continue
    if (to === undefined) {
      return refuse(`${d.usageType} ${fmtPrice(from)} is absent upstream`)
    }
    if (to <= 0) {
      return refuse(`${d.usageType} ${fmtPrice(from)} -> ${fmtPrice(to)} (zero or negative)`)
    }
    if (from <= 0) continue
    const ratio = to > from ? to / from : from / to
    if (ratio > MAX_PLAUSIBLE_RATIO * (1 + RATIO_EPSILON)) {
      return refuse(`${d.usageType} ${fmtPrice(from)} -> ${fmtPrice(to)} (${ratio.toFixed(1)}x)`)
    }
  }
  return { ok: true }
}

const refuse = (detail: string): GuardVerdict => ({
  ok: false,
  reason: `implausible delta: ${detail}`,
})

/** The price movement an action would actually cause — the input the guard must see.
 *
 * Every mutating action changes which definition prices this model, so every one is
 * guarded. What differs is WHICH pair to compare:
 *
 *   create / replace — the definition in force today becomes upstream's.
 *   delete (hand-back) — the override in force today is REMOVED, so pricing falls
 *     back to the managed row. Comparing the managed row against upstream here would
 *     be a guard that can never fire: this action is only reached BECAUSE those two
 *     already match. The change that takes effect is override -> managed, and
 *     deleting the row that was holding the old price applies the new one just as
 *     surely as writing it would.
 *   none — nothing takes effect, nothing to guard. */
export function guardDeltasFor(input: {
  action: Action
  upstream: UpstreamModel
  effManaged: InstanceModel | undefined
  effOverride: InstanceModel | undefined
}): PriceDelta[] {
  const { action, upstream, effManaged, effOverride } = input
  if (action.kind === 'none') return []
  if (action.kind === 'delete') {
    // Both are defined on this path by construction (a hand-back needs a managed row
    // that matches and an override to remove); an unexpected shape guards nothing
    // rather than inventing a comparison.
    return effOverride !== undefined && effManaged !== undefined
      ? diffPrices(effOverride, effManaged)
      : []
  }
  const inForce = effOverride ?? effManaged
  return inForce === undefined ? [] : diffPrices(inForce, upstream)
}

// ---------------------------------------------------------------------------
// Decision
// ---------------------------------------------------------------------------

/** The five-state reconciliation, over the two SELECTED rows (the one the server
 * would resolve within each partition) rather than the raw arrays. Comparing
 * against `managed.some(matches)` instead would hand a model back while the row
 * actually in force is still stale. Pure, so the states are directly testable. */
export function decideAction(input: {
  upstream: UpstreamModel
  effManaged: InstanceModel | undefined
  effOverride: InstanceModel | undefined
  overrideCount: number
}): Action {
  const { upstream, effManaged, effOverride } = input
  const managedMatches = effManaged !== undefined && diffPrices(effManaged, upstream).length === 0
  if (managedMatches) {
    return effOverride === undefined
      ? { kind: 'none', reason: `no drift (matched ${upstream.modelName})` }
      : { kind: 'delete', reason: 'managed row matches upstream; removing our override' }
  }
  if (effOverride === undefined) {
    return {
      kind: 'create',
      reason:
        effManaged === undefined
          ? 'no managed row matches; no existing override'
          : 'managed row is stale; no existing override',
    }
  }
  return diffPrices(effOverride, upstream).length === 0
    ? { kind: 'none', reason: 'override already matches upstream' }
    : { kind: 'replace', reason: 'override differs from upstream' }
}

/** The create body. Mirrors the tier array and the match pattern; the pattern is
 * what makes the judge's bare model string resolve at all, so it is written
 * byte-for-byte rather than reconstructed from the model name. Never the
 * deprecated flat price fields. */
export function buildCreateBody(upstream: UpstreamModel): CreateModelBody {
  return {
    modelName: upstream.modelName,
    matchPattern: upstream.matchPattern,
    unit: CREATE_UNIT,
    pricingTiers: upstream.pricingTiers.map(t => ({
      ...(t.name !== undefined ? { name: t.name } : {}),
      isDefault: t.isDefault,
      priority: t.priority,
      conditions: t.conditions,
      prices: t.prices,
    })),
    ...(upstream.tokenizerId !== undefined ? { tokenizerId: upstream.tokenizerId } : {}),
    ...(upstream.tokenizerConfig !== undefined
      ? { tokenizerConfig: upstream.tokenizerConfig }
      : {}),
  }
}

/** Delete order: every non-effective override FIRST, the effective one LAST.
 *
 * Load-bearing, not cosmetic. Deletes stop on the first failure, and that is only
 * SAFE if the surviving state still resolves to what it resolved to before the run.
 * With effective-last ordering any mid-sequence failure leaves the previously
 * effective override in place — genuinely no worse than before. Any other order can
 * delete the effective row and then fail, silently PROMOTING a dormant, different
 * override: a price change caused by a half-completed sync, presented as a clean
 * failure. */
export function orderedForDelete(
  overrides: InstanceModel[],
  effective: InstanceModel | undefined
): InstanceModel[] {
  if (effective === undefined) return [...overrides]
  return [
    ...overrides.filter(r => r.id !== effective.id),
    ...overrides.filter(r => r.id === effective.id),
  ]
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

/** Fetch the upstream price payload. Deliberately NOT `api()`: that helper
 * unconditionally attaches the Langfuse Basic Authorization header, which must
 * never be sent to this public third-party host, and it JSON-parses away the raw
 * body — but the provenance sha256 must cover exactly the bytes the server sent, so
 * the RAW text is returned and hashed BEFORE any parsing. Abortable, so a hung
 * fetch cannot stall the nightly round.
 * Do not "simplify" this into the shared helper. */
export async function fetchUpstream(
  url: string,
  timeoutMs: number,
  fetchImpl: typeof fetch = fetch
): Promise<{ text: string; sha256: string }> {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const res = await fetchImpl(url, { signal: controller.signal })
    const text = await res.text()
    if (!res.ok) {
      // A non-2xx is a failure, never an empty payload.
      throw new UpstreamFetchError(`GET ${url} -> ${res.status}: ${text.slice(0, 200)}`)
    }
    return { text, sha256: crypto.createHash('sha256').update(text).digest('hex') }
  } catch (err) {
    if (err instanceof UpstreamFetchError) throw err
    throw new UpstreamFetchError(
      `GET ${url} failed: ${err instanceof Error ? `${err.name}: ${err.message}` : String(err)}`
    )
  } finally {
    clearTimeout(timer)
  }
}

/** Every model definition on the instance. Walks EVERY page: a single-page read of
 * a ~170-definition instance makes anything on page 2 look absent, and "absent"
 * drives a create. */
export async function listModels(
  cfg: LangfuseConfig,
  timeoutMs: number = API_TIMEOUT_MS
): Promise<InstanceModel[]> {
  // The timeout bounds EVERY page request, not just the first: an unbounded page 2
  // would hang the round just as effectively as an unbounded page 1.
  const rows = await apiGetAllPages(cfg, '/api/public/models', 'page', timeoutMs)
  return rows.map((r, i) => parseInstanceRow(r, `models row ${i}`))
}

async function deleteModel(cfg: LangfuseConfig, id: string, timeoutMs: number): Promise<void> {
  await api(cfg, 'DELETE', `/api/public/models/${encodeURIComponent(id)}`, undefined, timeoutMs)
}

async function createModel(
  cfg: LangfuseConfig,
  body: CreateModelBody,
  timeoutMs: number
): Promise<void> {
  await api(cfg, 'POST', '/api/public/models', body, timeoutMs)
}

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

export interface Options {
  models?: string[]
  dryRun: boolean
  force: boolean
  strict: boolean
  upstreamFile?: string
  reset?: string
}

export function parseArgs(argv: string[]): Options | { usage: string } {
  const opts: Options = { dryRun: false, force: false, strict: false }
  let i = 0
  // A value that looks like another flag is NOT consumed: `--models --strict` is a
  // missing value, not a model named '--strict'.
  const takeValue = (): string | undefined => {
    const v = argv[i + 1]
    if (v === undefined || v.startsWith('--')) return undefined
    i++
    return v
  }
  while (i < argv.length) {
    const arg = argv[i]
    if (arg === '--dry-run') opts.dryRun = true
    else if (arg === '--force') opts.force = true
    else if (arg === '--strict') opts.strict = true
    else if (arg === '--models') {
      const v = takeValue()
      if (v === undefined) return { usage: '--models needs a comma-separated value' }
      opts.models = v
        .split(',')
        .map(s => s.trim())
        .filter(s => s.length > 0)
      if (opts.models.length === 0) return { usage: '--models needs at least one model' }
    } else if (arg === '--upstream') {
      const v = takeValue()
      if (v === undefined) return { usage: '--upstream needs a file path' }
      opts.upstreamFile = v
    } else if (arg === '--reset') {
      const v = takeValue()
      if (v === undefined) return { usage: '--reset needs a model string' }
      opts.reset = v
    } else return { usage: `unrecognized argument '${arg}'` }
    i++
  }
  return opts
}

const errMsg = (e: unknown): string => (e instanceof Error ? `${e.name}: ${e.message}` : String(e))

interface Tally {
  targets: number
  created: number
  replaced: number
  deleted: number
  refused: number
  absent: number
  failed: number
}

/** Per-call bounds. Injectable for the same reason the exporter's are: a test that
 * proves a call is bounded must not pay the production bound in wall clock. */
export interface Timeouts {
  upstreamMs: number
  apiMs: number
  healthMs: number
}

export interface MainDeps {
  fetchImpl?: typeof fetch
  log?: (s: string) => void
  errlog?: (s: string) => void
  readFile?: (p: string) => string
  timeouts?: Partial<Timeouts>
}

/** Non-blocking version skew check. The resolution ordering this script mirrors is
 * a property of the deployed code, not of the documented contract, so a different
 * version is the moment to re-verify it — surfaced in the nightly log rather than
 * left as a source comment nobody reads. A failed health read is itself only a
 * warning: it says nothing about the prices. */
async function warnOnVersionSkew(
  cfg: LangfuseConfig,
  errlog: (s: string) => void,
  timeoutMs: number
): Promise<void> {
  try {
    const health = await api(cfg, 'GET', '/api/public/health', undefined, timeoutMs)
    const version = typeof health.version === 'string' ? health.version : undefined
    if (version === undefined) {
      errlog('qa-model-prices: WARNING health check reported no version — cannot check for skew')
      return
    }
    if (version !== VERIFIED_LANGFUSE_VERSION) {
      errlog(
        `qa-model-prices: WARNING Langfuse version ${version} differs from the verified ` +
          `${VERIFIED_LANGFUSE_VERSION} — re-verify the server's model-price resolution ` +
          'ordering before trusting this sync'
      )
    }
  } catch (err) {
    errlog(`qa-model-prices: WARNING health check failed (${errMsg(err)}) — skew unchecked`)
  }
}

/** Reconcile ONE target against the instance snapshot taken at the start of the run.
 *
 * `reconciled` maps an upstream modelName to the target that already reconciled it.
 * DIFFERENT target strings can select the SAME upstream definition (a bare alias and
 * a dated one — `gpt-5.5`'s live pattern matches both `gpt-5.5` and
 * `gpt-5.5-2026-04-23`), and `activeModels()` cannot see that: it dedupes identical
 * strings, not distinct strings resolving to one model. Reconciling the second one
 * against the same (now stale) snapshot would POST a duplicate modelName or DELETE an
 * already-removed id and report a failure that is purely an artifact of the run.
 *
 * Deduping is chosen over re-reading the instance between targets: the second pass
 * over one definition has nothing left to do by construction, so a re-read would cost
 * a full paginated walk per target to discover exactly that. The alias is still
 * reported on its own line, so every requested target is accounted for. */
async function reconcileTarget(
  cfg: LangfuseConfig,
  target: string,
  upstreamModels: UpstreamModel[],
  rows: InstanceModel[],
  opts: Options,
  tally: Tally,
  log: (s: string) => void,
  apiMs: number,
  reconciled: Map<string, string>
): Promise<void> {
  const line = (action: string, reason: string): void =>
    log(`model=${target} action=${action} reason=${reason}`)

  let upstream: UpstreamModel
  try {
    upstream = selectUpstream(upstreamModels, target)
  } catch (err) {
    if (err instanceof NotFoundUpstreamError) {
      // Renamed or removed upstream. The existing definition is left ALONE.
      tally.absent++
      line('absent', `${err.message}; existing definition left untouched`)
      return
    }
    tally.refused++
    line('refused', errMsg(err))
    return
  }

  const alias = reconciled.get(upstream.modelName)
  if (alias !== undefined) {
    line('none', `already reconciled as '${alias}' (both select ${upstream.modelName})`)
    return
  }
  reconciled.set(upstream.modelName, target)

  let managed: InstanceModel[]
  let overrides: InstanceModel[]
  let effManaged: InstanceModel | undefined
  let effOverride: InstanceModel | undefined
  try {
    const split = splitInstanceRows(rows, target)
    managed = split.managed
    overrides = split.overrides
    effManaged = effectiveWithin(managed)
    effOverride = effectiveWithin(overrides)
  } catch (err) {
    tally.refused++
    line('refused', err instanceof AmbiguousTieError ? `ambiguous: ${err.message}` : errMsg(err))
    return
  }

  // Project rows carrying the SAME modelName whose pattern no longer matches the
  // target. They do not serve the target and so never influence the decision, but
  // they DO block a create on the (project, modelName) uniqueness — leaving one in
  // place deadlocks the model permanently, since every future create 400s.
  const blocking = blockingOverrides(rows, target, upstream.modelName)

  // More than one project-scoped row for one target means something outside this
  // sync wrote one — say so before touching anything.
  if (overrides.length > 1) {
    log(`model=${target} note=${overrides.length} project-scoped override rows match this target`)
  }
  if (blocking.length > 0) {
    log(
      `model=${target} note=${blocking.length} project-scoped row(s) carry modelName ` +
        `${upstream.modelName} with a non-matching pattern; removing them so a create is not rejected`
    )
  }

  const action = decideAction({
    upstream,
    effManaged,
    effOverride,
    overrideCount: overrides.length,
  })

  if (action.kind === 'none') {
    line('none', action.reason)
    return
  }

  // EVERY mutating action is guarded, including the hand-back: deleting the row that
  // was holding the old price APPLIES the new one just as surely as writing it.
  const verdict = guardDelta(guardDeltasFor({ action, upstream, effManaged, effOverride }))
  if (!verdict.ok && !opts.force) {
    tally.refused++
    line('refused', verdict.reason ?? 'implausible delta')
    return
  }

  if (opts.dryRun) {
    // Same action word as a real run so the two are directly comparable; the reason
    // carries the "nothing written" part.
    line(action.kind, `${action.reason} (dry-run, nothing written)`)
    return
  }

  // Blocking rows are non-effective for the target by construction, so ordering them
  // ahead of the effective override preserves the effective-last contract.
  const ordered = orderedForDelete([...overrides, ...blocking], effOverride)
  let deleted = 0
  for (const row of ordered) {
    try {
      await deleteModel(cfg, row.id, apiMs)
      deleted++
    } catch (err) {
      // STOP: no further deletes, and no create. With effective-last ordering the
      // previously effective override is still in place, so the model resolves
      // exactly as it did before the run. Creating now would be the dangerous
      // option — a surviving dated override outranks a new undated row, so the run
      // would report success while the stale row stayed in force.
      tally.failed++
      line(
        'failed',
        `deleted ${deleted} of ${ordered.length} override(s), delete failed: ${errMsg(err)}`
      )
      return
    }
  }

  if (action.kind === 'delete') {
    tally.deleted += deleted
    line('delete', `${action.reason} (deleted ${plural(deleted, 'override')})`)
    return
  }

  try {
    await createModel(cfg, buildCreateBody(upstream), apiMs)
  } catch (err) {
    tally.failed++
    // No rollback, deliberately: re-creating the old override against an API that
    // is already failing is likelier to compound the failure than repair it, and
    // reconciliation converges — the next run reaches the right state from whatever
    // it observes.
    if (managed.length === 0) {
      // Nothing is left to price this model AT ALL: every observation for it prices
      // at zero until a later run re-creates the override, and the nightly's
      // fail-open means this line is the only signal.
      line(
        'failed',
        `deleted ${plural(deleted, 'override')}, create failed: ${errMsg(err)}; ` +
          'NO definition remains, model is now UNPRICED'
      )
    } else {
      // The model falls back to its managed row — stale, but never a wrong custom
      // price, and exactly where it would have been without this sync.
      line('failed', `deleted ${plural(deleted, 'override')}, create failed: ${errMsg(err)}`)
    }
    return
  }

  if (action.kind === 'replace') {
    tally.replaced++
    line('replace', `deleted ${plural(deleted, 'override')}, recreated`)
  } else {
    tally.created++
    line('create', action.reason)
  }
}

const plural = (n: number, word: string): string => `${n} ${word}${n === 1 ? '' : 's'}`

/** --reset: delete this project's override(s) for one model and stop.
 *
 * Reconciliation already hands a model back automatically once the managed row
 * catches up; this is the immediate operational version, and the cleanup after a
 * live write-path smoke.
 *
 * DELIBERATELY NOT guarded by the implausible-delta check, unlike every action the
 * reconciler takes. That guard exists so a large price movement cannot be applied
 * UNATTENDED; this path is a human naming one model and asking for its override to
 * go now. Requiring --force here would only teach the operator to pass it by habit.
 *
 * Rows are matched by pattern OR by exact modelName, so an override written under a
 * pattern that upstream has since changed is still cleaned up — the same row a
 * pattern-only search would strand. */
async function runReset(
  cfg: LangfuseConfig,
  target: string,
  log: (s: string) => void,
  errlog: (s: string) => void,
  apiMs: number
): Promise<number> {
  const rows = await listModels(cfg, apiMs)
  const { overrides } = splitInstanceRows(rows, target)
  const byName = blockingOverrides(rows, target, target)
  let effective: InstanceModel | undefined
  try {
    effective = effectiveWithin(overrides)
  } catch {
    // Ambiguous ties do not matter here: every matching override is going away, so
    // there is no "which one resolves" question left to answer.
    effective = undefined
  }
  const ordered = orderedForDelete([...overrides, ...byName], effective)
  let deleted = 0
  for (const row of ordered) {
    try {
      await deleteModel(cfg, row.id, apiMs)
      deleted++
    } catch (err) {
      errlog(
        `qa-model-prices: reset ${target}: deleted ${deleted} of ${ordered.length} ` +
          `override(s), delete failed: ${errMsg(err)}`
      )
      return EXIT.FAIL
    }
  }
  log(`reset ${target}: deleted ${plural(deleted, 'project override')}`)
  return EXIT.OK
}

export async function main(
  argv: string[],
  env: Record<string, string | undefined> = process.env,
  deps: MainDeps = {}
): Promise<number> {
  const log = deps.log ?? ((s: string) => console.log(s))
  const errlog = deps.errlog ?? ((s: string) => console.error(s))
  const readFile = deps.readFile ?? ((p: string) => fs.readFileSync(p, 'utf8'))
  const upstreamMs = deps.timeouts?.upstreamMs ?? UPSTREAM_TIMEOUT_MS
  const apiMs = deps.timeouts?.apiMs ?? API_TIMEOUT_MS
  const healthMs = deps.timeouts?.healthMs ?? HEALTH_TIMEOUT_MS

  const parsed = parseArgs(argv)
  if ('usage' in parsed) {
    // A misinvocation is a caller error, never something to fail open on.
    errlog(`qa-model-prices: ${parsed.usage}`)
    return EXIT.USAGE
  }
  const opts = parsed
  // Fail-open is the NIGHTLY's job (it keeps running the round); --strict makes the
  // exit code trustworthy for a human bakeoff and for the nightly's own reporting.
  const failCode = opts.strict ? EXIT.FAIL : EXIT.OK

  const cfg = configFromEnv(env)
  if (cfg === undefined) {
    errlog(
      'qa-model-prices: LANGFUSE_HOST/LANGFUSE_PUBLIC_KEY/LANGFUSE_SECRET_KEY are not all set — ' +
        'nothing synced'
    )
    return failCode
  }

  await warnOnVersionSkew(cfg, errlog, healthMs)

  if (opts.reset !== undefined) {
    try {
      return await runReset(cfg, opts.reset, log, errlog, apiMs)
    } catch (err) {
      errlog(`qa-model-prices: reset ${opts.reset} failed: ${errMsg(err)}`)
      // An operational cleanup that silently "succeeded" would be a footgun, so
      // reset reports failure regardless of --strict.
      return EXIT.FAIL
    }
  }

  const targets = opts.models ?? activeModels(env)

  // Fetch + validate BEFORE any write: a payload that is wrong anywhere writes
  // nothing anywhere.
  let text: string
  let sha256: string
  try {
    if (opts.upstreamFile !== undefined) {
      text = readFile(opts.upstreamFile)
      sha256 = crypto.createHash('sha256').update(text).digest('hex')
    } else {
      const fetched = await fetchUpstream(UPSTREAM_URL, upstreamMs, deps.fetchImpl)
      text = fetched.text
      sha256 = fetched.sha256
    }
  } catch (err) {
    errlog(`qa-model-prices: upstream unavailable: ${errMsg(err)}`)
    return failCode
  }

  let upstreamModels: UpstreamModel[]
  try {
    upstreamModels = parseUpstream(text)
  } catch (err) {
    errlog(`qa-model-prices: upstream payload rejected: ${errMsg(err)}`)
    return failCode
  }

  let rows: InstanceModel[]
  try {
    rows = await listModels(cfg, apiMs)
  } catch (err) {
    errlog(`qa-model-prices: could not read the instance's model definitions: ${errMsg(err)}`)
    return failCode
  }

  const tally: Tally = {
    targets: targets.length,
    created: 0,
    replaced: 0,
    deleted: 0,
    refused: 0,
    absent: 0,
    failed: 0,
  }
  const reconciled = new Map<string, string>()
  for (const target of targets) {
    try {
      await reconcileTarget(cfg, target, upstreamModels, rows, opts, tally, log, apiMs, reconciled)
    } catch (err) {
      // One target's unexpected failure never takes the others down with it.
      tally.failed++
      log(`model=${target} action=failed reason=${errMsg(err)}`)
    }
  }

  // Every count reports what actually happened: refusals, absences, and failures
  // are named here rather than folded into a success total.
  log(
    `summary: ${tally.targets} targets, ${tally.created} created, ${tally.replaced} replaced, ` +
      `${tally.deleted} deleted, ${tally.refused} refused, ${tally.absent} absent, ` +
      `${tally.failed} failed`
  )
  log(`upstream_sha256=${sha256}`)

  const clean = tally.refused === 0 && tally.absent === 0 && tally.failed === 0
  return clean ? EXIT.OK : failCode
}

// Import-guarded: importing this module (as the test does) runs NO side effects.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main(process.argv.slice(2)).then(code => {
    process.exitCode = code
  })
}
