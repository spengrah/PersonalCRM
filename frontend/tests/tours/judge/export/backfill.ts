// CLI: qa-fn-backfill — turn a downstream bug discovery into recall signal.
//
//   list:    bun run tests/tours/judge/export/backfill.ts <behavior_id> [--round <runId|gitSha>]
//   enqueue: bun run tests/tours/judge/export/backfill.ts <behavior_id> <traceId>
//
// The list form prints the covering-PASS candidate traces for a behavior — a deep
// link plus the judge's cite/critique — so a maintainer can eyeball the one that
// covers the bug they found elsewhere. The enqueue form adds a proven candidate to
// the standing `qa-triage` queue, where it is scored `should_fail` in the normal
// flow. That queue is the ONLY write path (no direct score) so the human-triage
// boundary is never bypassed.
//
// FAIL-CLOSED (opposite of qa-export's best-effort): because it writes into the
// false-negative ground-truth pool, EVERY failure — a missing/ambiguous queue, any
// list GET / pagination error, an ambiguous verdict, a non-member traceId, an
// unknown behavior, an unresolvable deep-link project, or the item POST itself —
// exits NON-ZERO and never reports the backfill as done. It is a REPORTER, not a
// status-mutator: an already-triaged item is reported for a manual UI reopen, never
// PATCHed.

import {
  ApiError,
  PaginationError,
  api,
  apiGetAllPages,
  configFromEnv,
  type LangfuseConfig,
} from './langfuse'
import { validGitSha, validRunId } from './run'
import { TRIAGE_QUEUE_NAME, VERDICT_SCORE_NAME } from './triage-config'
import { SPEC_CATALOG } from '../spec-catalog'
import { INTENT_CATALOG } from '../intent-catalog'

type Verdict = 'pass' | 'fail' | 'unsure'

// A spec-invalid 2xx wire row (a verdict score or queue item the API returned but that
// violates its own schema — a malformed subject, a non-enum value). A fail-CLOSED
// writer must NOT silently drop such a row to "absent" — that would fail OPEN (a
// malformed score makes a trace look scoreless → the output-PASS fallback admits it;
// a malformed existing item makes a queued trace look un-queued → a duplicate POST).
// Thrown so the caller exits non-zero rather than acting on an untrustworthy read.
class BackfillDataError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'BackfillDataError'
  }
}

// Distinct exit codes so the three not-found list outcomes are separable by a
// script/human. FAIL is the single fail-closed operational code; USAGE covers bad
// args + an unknown behavior (a typo/coverage error, caught before any query).
const EXIT = { OK: 0, FAIL: 1, USAGE: 2, NO_TRACES: 3, NO_COVERING_PASS: 4 } as const

// Langfuse v3 closed variant/enum sets (from the deployed OpenAPI: ScoreSubjectV3,
// AnnotationQueueObjectType, AnnotationQueueStatus). For every closed discriminator on a
// 2xx row, a value OUTSIDE its set — INCLUDING missing — is spec-invalid and fails CLOSED;
// only a genuinely-valid-but-not-our-target value (e.g. an observation-kind score, a
// SESSION item) is legitimately skipped. A silently-ignored corrupt row fails OPEN (the
// row vanishes → a scored trace looks scoreless / a queued trace looks un-queued).
const SUBJECT_KINDS: ReadonlySet<string> = new Set([
  'trace',
  'observation',
  'session',
  'experiment',
])
const QUEUE_OBJECT_TYPES: ReadonlySet<string> = new Set(['TRACE', 'OBSERVATION', 'SESSION'])
const QUEUE_ITEM_STATUSES: ReadonlySet<string> = new Set(['PENDING', 'COMPLETED'])

// The behavior/intent registry — the SAME catalogs the judge + tours use, so a trace
// tagged `behavior:<id>` (the tag comes from `qa.behavior_id`, which is a behavior id
// for residue behaviors and an intent id for intent judgments). This is what makes
// "unknown behavior" (id absent — a typo) distinguishable from "valid behavior, zero
// traces" (a coverage gap): Langfuse data alone cannot tell them apart.
const BEHAVIOR_REGISTRY: ReadonlySet<string> = new Set([
  ...Object.keys(SPEC_CATALOG),
  ...Object.keys(INTENT_CATALOG),
])

const errMsg = (e: unknown): string => (e instanceof Error ? e.message : String(e))

const asVerdict = (v: unknown): Verdict | undefined =>
  v === 'pass' || v === 'fail' || v === 'unsure' ? v : undefined

// A graded item-trace's output is its PerItemVerdict ({verdict, citation, critique});
// a content-light trace's output is a string/undefined (no verdict).
function outputVerdict(output: unknown): Verdict | undefined {
  if (output !== null && typeof output === 'object' && !Array.isArray(output)) {
    return asVerdict((output as { verdict?: unknown }).verdict)
  }
  return undefined
}

function citeCritique(output: unknown): { citation?: string; critique?: string } {
  if (output !== null && typeof output === 'object' && !Array.isArray(output)) {
    const o = output as { citation?: unknown; critique?: unknown }
    return {
      citation: typeof o.citation === 'string' ? o.citation : undefined,
      critique: typeof o.critique === 'string' ? o.critique : undefined,
    }
  }
  return {}
}

// Closed, fail-safe per-trace classification from its verdict scores + output:
//   >1 verdict scores → ambiguous (never choose one; fail closed)
//    1 verdict score  → `pass` qualifies; `fail`/`unsure`/other excludes even if the
//                       trace OUTPUT says pass (the bound score wins)
//    0 verdict scores → fall back to output PASS (a legacy trace or a failed score
//                       request has none; keyed on zero PRESENCE, never on absence
//                       from a pass-filtered query)
type Classification = 'candidate' | 'excluded' | 'ambiguous'
function classify(verdictScores: Verdict[], output: unknown): Classification {
  if (verdictScores.length > 1) return 'ambiguous'
  if (verdictScores.length === 1) return verdictScores[0] === 'pass' ? 'candidate' : 'excluded'
  return outputVerdict(output) === 'pass' ? 'candidate' : 'excluded'
}

interface Candidate {
  traceId: string
  citation?: string
  critique?: string
}
interface CandidateSet {
  traceCount: number
  candidates: Candidate[]
  ambiguous: string[]
}

// Build a behavior's candidate set: traces tagged `behavior:<id>` (+ an optional round
// tag, AND-combined), each classified against ALL verdict scores. Throws ApiError /
// PaginationError on any list/pagination failure (fail-closed at the call site).
async function buildCandidateSet(
  cfg: LangfuseConfig,
  behaviorId: string,
  roundTag: string | undefined
): Promise<CandidateSet> {
  // Traces filtered by tag — `tags` is an AND filter, so behavior + round both apply.
  // `fields=core,io` returns `output` (the verdict/cite/critique) alongside the id/tags.
  const tParams = new URLSearchParams()
  tParams.append('tags', `behavior:${behaviorId}`)
  if (roundTag) tParams.append('tags', roundTag)
  tParams.set('fields', 'core,io')
  const traces = await apiGetAllPages(cfg, `/api/public/traces?${tParams.toString()}`, 'page')

  // ALL verdict scores (no `value` filter — a pass-only query cannot tell a hidden
  // fail/unsure from a missing score). `fields=subject` is required because core score
  // rows omit the trace association. Scores v3 is CURSOR-paginated.
  const sParams = new URLSearchParams()
  sParams.set('name', VERDICT_SCORE_NAME)
  sParams.set('dataType', 'CATEGORICAL')
  sParams.set('fields', 'subject')
  const scores = await apiGetAllPages(cfg, `/api/public/v3/scores?${sParams.toString()}`, 'cursor')

  // Join by the trace-level subject. Langfuse v3 puts the trace id in `subject.id` when
  // `subject.kind === 'trace'` (only OBSERVATION-level subjects carry `subject.traceId`),
  // so a trace-bound verdict score joins on `subject.id`. Non-trace subjects (observation/
  // session/experiment) are legitimately IGNORED — they are not this trace's verdict. But a
  // TRACE-kind row that is spec-invalid (no `subject.id`, or a value outside the config's
  // {pass,fail,unsure}) is a corrupt read → fail CLOSED, never dropped to "scoreless".
  const byTrace = new Map<string, Verdict[]>()
  for (const s of scores) {
    const subject = (s as { subject?: unknown }).subject
    if (subject === null || typeof subject !== 'object' || Array.isArray(subject)) {
      throw new BackfillDataError(
        `verdict score '${String((s as { id?: unknown }).id)}' has no subject`
      )
    }
    const sub = subject as { kind?: unknown; id?: unknown }
    if (sub.kind !== 'trace') {
      // A VALID non-trace variant (observation/session/experiment) is legitimately not
      // this trace's verdict; a MISSING or UNKNOWN kind is a corrupt row → fail closed.
      if (typeof sub.kind === 'string' && SUBJECT_KINDS.has(sub.kind)) continue
      throw new BackfillDataError(
        `verdict score '${String((s as { id?: unknown }).id)}' has a missing/unknown ` +
          `subject.kind (${String(sub.kind)})`
      )
    }
    if (typeof sub.id !== 'string' || sub.id.length === 0) {
      throw new BackfillDataError(
        `trace-level verdict score '${String((s as { id?: unknown }).id)}' has no subject.id`
      )
    }
    const value = asVerdict((s as { value?: unknown }).value)
    if (!value) {
      throw new BackfillDataError(
        `verdict score for trace ${sub.id} has a non-{pass,fail,unsure} value ` +
          `(${String((s as { value?: unknown }).value)})`
      )
    }
    const arr = byTrace.get(sub.id) ?? []
    arr.push(value)
    byTrace.set(sub.id, arr)
  }

  const candidates: Candidate[] = []
  const ambiguous: string[] = []
  for (const t of traces) {
    // The paginator guarantees a string id on every item.
    const traceId = t.id as string
    const output = (t as { output?: unknown }).output
    const c = classify(byTrace.get(traceId) ?? [], output)
    if (c === 'ambiguous') ambiguous.push(traceId)
    else if (c === 'candidate') candidates.push({ traceId, ...citeCritique(output) })
  }
  return { traceCount: traces.length, candidates, ambiguous }
}

// The UI project id for a trace deep link. Deterministic + fail-closed: env override,
// else the key's SOLE project; zero/multiple/error → throw (never guess a link).
async function resolveProjectId(
  cfg: LangfuseConfig,
  env: Record<string, string | undefined>
): Promise<string> {
  const fromEnv = env.LANGFUSE_PROJECT_ID
  if (fromEnv) return fromEnv
  const res = await api(cfg, 'GET', '/api/public/projects')
  const data = (res as { data?: unknown }).data
  if (!Array.isArray(data) || data.length !== 1) {
    throw new Error(`expected exactly 1 project, found ${Array.isArray(data) ? data.length : 0}`)
  }
  const id = (data[0] as { id?: unknown }).id
  if (typeof id !== 'string' || id.length === 0) throw new Error('project has no id')
  return id
}

async function runList(
  cfg: LangfuseConfig,
  env: Record<string, string | undefined>,
  behaviorId: string,
  roundTag: string | undefined,
  log: (msg: string) => void,
  errlog: (msg: string) => void
): Promise<number> {
  const set = await buildCandidateSet(cfg, behaviorId, roundTag)
  if (set.ambiguous.length > 0) {
    errlog(
      `qa-fn-backfill: multiple verdict scores for trace(s) ${set.ambiguous.join(', ')} — ` +
        'ambiguous; resolve in the Langfuse UI. Not listing candidates.'
    )
    return EXIT.FAIL
  }
  const round = roundTag ? ` (${roundTag})` : ''
  if (set.traceCount === 0) {
    errlog(
      `qa-fn-backfill: no traces tagged behavior:${behaviorId}${round} — coverage gap ` +
        '(extend the tour), not a judge miss.'
    )
    return EXIT.NO_TRACES
  }
  if (set.candidates.length === 0) {
    errlog(
      `qa-fn-backfill: ${set.traceCount} trace(s) for ${behaviorId}${round} but none is a ` +
        'covering PASS — nothing to backfill.'
    )
    return EXIT.NO_COVERING_PASS
  }

  let projectId: string
  try {
    projectId = await resolveProjectId(cfg, env)
  } catch (err) {
    errlog(
      `qa-fn-backfill: cannot resolve a deep-link project (${errMsg(err)}) — ` +
        'set LANGFUSE_PROJECT_ID.'
    )
    return EXIT.FAIL
  }

  log(`${set.candidates.length} covering-PASS candidate(s) for ${behaviorId}${round}:`)
  for (const c of set.candidates) {
    log(`  ${c.traceId}`)
    log(`    ${cfg.host}/project/${projectId}/traces/${c.traceId}`)
    if (c.citation) log(`    cite: ${c.citation}`)
    if (c.critique) log(`    critique: ${c.critique}`)
  }
  log(`\nto backfill one: qa-fn-backfill ${behaviorId} <traceId>`)
  return EXIT.OK
}

async function runEnqueue(
  cfg: LangfuseConfig,
  behaviorId: string,
  traceId: string,
  log: (msg: string) => void,
  errlog: (msg: string) => void
): Promise<number> {
  // Membership proof: the trace must cover this behavior AND be a current covering PASS.
  // Built WITHOUT a round narrow (membership is round-agnostic).
  const set = await buildCandidateSet(cfg, behaviorId, undefined)
  if (set.ambiguous.length > 0) {
    errlog(
      `qa-fn-backfill: multiple verdict scores for trace(s) ${set.ambiguous.join(', ')} — ` +
        'ambiguous; resolve in the Langfuse UI. Not enqueuing.'
    )
    return EXIT.FAIL
  }
  if (!set.candidates.some(c => c.traceId === traceId)) {
    errlog(
      `qa-fn-backfill: trace ${traceId} is not a covering-PASS candidate for ${behaviorId} — ` +
        'refusing to enqueue (guards the false-negative ground-truth pool).'
    )
    return EXIT.FAIL
  }

  // Resolve the standing queue (exactly one) — fail closed on missing/ambiguous.
  const queues = await apiGetAllPages(cfg, '/api/public/annotation-queues', 'page')
  const matches = queues.filter(q => q.name === TRIAGE_QUEUE_NAME)
  if (matches.length !== 1 || typeof matches[0].id !== 'string') {
    errlog(
      `qa-fn-backfill: expected exactly 1 '${TRIAGE_QUEUE_NAME}' queue, found ${matches.length} — ` +
        'cannot enqueue.'
    )
    return EXIT.FAIL
  }
  const queueId = matches[0].id as string

  // Existing items keyed by (objectType, objectId): queue-item POST has no server dedup,
  // so REPORT rather than blindly re-POST. A correct dedup read requires EVERY item's
  // identity to be well-formed — a malformed objectType/objectId would let a real match
  // slip through and duplicate the POST, so a spec-invalid item fails CLOSED.
  const items = await apiGetAllPages(cfg, `/api/public/annotation-queues/${queueId}/items`, 'page')
  for (const it of items) {
    const objectType = (it as { objectType?: unknown }).objectType
    const objectId = (it as { objectId?: unknown }).objectId
    if (typeof objectId !== 'string') {
      throw new BackfillDataError(
        `queue item '${String((it as { id?: unknown }).id)}' has a malformed objectId`
      )
    }
    // objectType must be a KNOWN enum member: a wrong-case 'trace' or garbage would let a
    // real TRACE match slip through → a duplicate POST. Fail closed instead.
    if (typeof objectType !== 'string' || !QUEUE_OBJECT_TYPES.has(objectType)) {
      throw new BackfillDataError(
        `queue item '${String((it as { id?: unknown }).id)}' has a missing/unknown ` +
          `objectType (${String(objectType)})`
      )
    }
  }
  const matching = items.filter(
    it => (it as { objectType: string }).objectType === 'TRACE' && it.objectId === traceId
  )
  if (matching.length > 1) {
    const detail = matching
      .map(it => `${String(it.id)}:${String((it as { status?: unknown }).status)}`)
      .join(', ')
    errlog(
      `qa-fn-backfill: trace ${traceId} has ${matching.length} matching queue items (${detail}) — ` +
        'ambiguous; resolve in the Langfuse UI. Not enqueuing.'
    )
    return EXIT.FAIL
  }
  if (matching.length === 1) {
    const rawStatus = (matching[0] as { status?: unknown }).status
    // status is a closed enum {PENDING, COMPLETED}: a missing/unknown value is spec-invalid,
    // not "neither pending nor completed" → fail closed.
    if (typeof rawStatus !== 'string' || !QUEUE_ITEM_STATUSES.has(rawStatus)) {
      throw new BackfillDataError(
        `queue item for trace ${traceId} has a missing/unknown status (${String(rawStatus)})`
      )
    }
    const status = rawStatus
    if (status === 'PENDING') {
      log(`qa-fn-backfill: trace ${traceId} already queued (pending) in ${TRIAGE_QUEUE_NAME}.`)
      return EXIT.OK
    }
    errlog(
      `qa-fn-backfill: trace ${traceId} already triaged (${status}); reopen it in the Langfuse ` +
        'UI to re-triage. Not re-enqueuing.'
    )
    return EXIT.FAIL
  }

  // Zero existing → POST. A non-2xx throws ApiError → caught by main → non-zero.
  await api(cfg, 'POST', `/api/public/annotation-queues/${queueId}/items`, {
    objectId: traceId,
    objectType: 'TRACE',
  })
  log(`qa-fn-backfill: enqueued ${traceId} into ${TRIAGE_QUEUE_NAME}.`)
  return EXIT.OK
}

export async function main(
  argv: string[],
  env: Record<string, string | undefined> = process.env,
  log: (msg: string) => void = console.log,
  errlog: (msg: string) => void = console.error
): Promise<number> {
  const cfg = configFromEnv(env)
  if (!cfg) {
    errlog(
      'qa-fn-backfill: LANGFUSE_HOST / LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY not set — ' +
        'cannot reach Langfuse.'
    )
    return EXIT.USAGE
  }

  const usage =
    'usage: qa-fn-backfill <behavior_id> [--round <runId|gitSha>]   (list)\n' +
    '       qa-fn-backfill <behavior_id> <traceId>                 (enqueue)'

  const behaviorId = argv[0]
  if (!behaviorId) {
    errlog(usage)
    return EXIT.USAGE
  }

  // Format-dispatch + STRICT arity BEFORE any query — a fail-closed writer rejects any
  // invocation that is not EXACTLY a recognized form (no ignored trailing args, no
  // list+enqueue mix). Recognized:
  //   list:    <behavior>                   (rest = [])
  //   list:    <behavior> --round <value>   (rest = ['--round', value])
  //   enqueue: <behavior> <traceId>         (rest = [traceId])
  const rest = argv.slice(1)
  let roundTag: string | undefined
  let traceIdArg: string | undefined
  if (rest.length === 0) {
    // list, no round
  } else if (rest[0] === '--round') {
    if (rest.length !== 2) {
      errlog(`qa-fn-backfill: --round takes exactly one value and no other arguments.\n${usage}`)
      return EXIT.USAGE
    }
    const r = rest[1]
    if (validRunId(r)) roundTag = `runId:${r}`
    else if (validGitSha(r)) roundTag = `gitSha:${r}`
    else {
      errlog(
        `qa-fn-backfill: --round '${r}' is neither a run-id (YYYYMMDDTHHMMSSZ) nor a ` +
          'git sha (7-40 hex).'
      )
      return EXIT.USAGE
    }
  } else if (rest.length === 1 && !rest[0].startsWith('--')) {
    traceIdArg = rest[0]
  } else {
    errlog(`qa-fn-backfill: unrecognized arguments.\n${usage}`)
    return EXIT.USAGE
  }

  // An unknown behavior is a typo/coverage error — reject before any query.
  if (!BEHAVIOR_REGISTRY.has(behaviorId)) {
    errlog(
      `qa-fn-backfill: unknown behavior '${behaviorId}' — not in the behavior/intent catalog ` +
        '(a typo, or a behavior the judge does not cover).'
    )
    return EXIT.USAGE
  }

  try {
    return traceIdArg === undefined
      ? await runList(cfg, env, behaviorId, roundTag, log, errlog)
      : await runEnqueue(cfg, behaviorId, traceIdArg, log, errlog)
  } catch (err) {
    if (
      err instanceof ApiError ||
      err instanceof PaginationError ||
      err instanceof BackfillDataError
    ) {
      errlog(`qa-fn-backfill: ${err.name}: ${err.message}`)
      return EXIT.FAIL
    }
    throw err
  }
}

// Import-guarded: importing this module (as backfill.test.ts does) runs NO side
// effects; main() executes only when the file is the process entry point.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main(process.argv.slice(2)).then(code => {
    process.exitCode = code
  })
}
