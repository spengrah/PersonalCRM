// CLI: idempotently provision the standing QA triage substrate in Langfuse.
//
//   LANGFUSE_HOST=... LANGFUSE_PUBLIC_KEY=... LANGFUSE_SECRET_KEY=... \
//     bun run tests/tours/judge/export/setup.ts
//
// It reconciles the DESIRED state: the `verdict` / `ground_truth` / `disposition`
// score configs and the standing `qa-triage` annotation queue (editable dims =
// ground_truth + disposition). Re-running a correctly-provisioned tenant creates
// nothing and exits 0.
//
// FAIL-CLOSED, unlike qa-export: this is an explicitly-invoked provisioning command
// whose whole job is to establish a runtime prerequisite, so missing creds or any
// drift exits NON-ZERO — a silent no-op would manufacture false confidence. It is
// also OPERATOR-CONTROLLED: it never MUTATES an existing object (Langfuse supports
// score-config updates, but auto-rewriting shared triage infra is declined) — on any
// drift it reports the diff and exits non-zero for a human to resolve.

import {
  ApiError,
  api,
  apiGetAllPages,
  configFromEnv,
  PaginationError,
  type LangfuseConfig,
} from './langfuse'
import {
  SCORE_CONFIGS,
  TRIAGE_QUEUE_NAME,
  TRIAGE_QUEUE_SCORE_CONFIGS,
  type ScoreCategory,
  type ScoreConfigSpec,
} from './triage-config'

interface ScoreConfig {
  id: string
  name: string
  dataType: string
  isArchived: boolean
  categories?: ScoreCategory[] | null
}

interface AnnotationQueue {
  id: string
  name: string
  scoreConfigIds: string[]
}

// Group already-deduped-by-id objects by name. More than one DISTINCT id under one
// name is a prior double-create / hand-made dup → caught as drift by the caller.
function groupByName<T extends { name: string }>(objs: T[]): Map<string, T[]> {
  const byName = new Map<string, T[]>()
  for (const o of objs) {
    const list = byName.get(o.name)
    if (list) list.push(o)
    else byName.set(o.name, [o])
  }
  return byName
}

// A config matches its spec iff dataType + the exact category set agree AND it is
// NOT archived (isArchived lives on the score-config, per the v3 response).
function scoreConfigDrift(spec: ScoreConfigSpec, actual: ScoreConfig): string | undefined {
  if (actual.isArchived) return `score-config '${spec.name}' is archived (isArchived=true)`
  if (actual.dataType !== spec.dataType) {
    return `score-config '${spec.name}' dataType ${actual.dataType} != ${spec.dataType}`
  }
  if (!categoriesMatch(spec.categories, actual.categories)) {
    return `score-config '${spec.name}' categories differ from desired`
  }
  return undefined
}

// Set equality keyed by label→value (order-independent, exact count).
function categoriesMatch(
  desired: ScoreCategory[],
  actual: ScoreCategory[] | null | undefined
): boolean {
  if (!Array.isArray(actual) || actual.length !== desired.length) return false
  const want = new Map(desired.map(c => [c.label, c.value]))
  for (const c of actual) {
    if (!want.has(c.label) || want.get(c.label) !== c.value) return false
  }
  return true
}

export async function main(
  env: Record<string, string | undefined> = process.env,
  log: (msg: string) => void = console.log,
  errlog: (msg: string) => void = console.error
): Promise<number> {
  const cfg = configFromEnv(env)
  if (!cfg) {
    errlog(
      'qa-langfuse-setup: LANGFUSE_HOST / LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY not set — ' +
        'cannot provision the triage substrate. Set them and re-run.'
    )
    return 1
  }

  try {
    // --- read current state (paginated, page protocol) ---
    const existingConfigs = (await apiGetAllPages(
      cfg,
      '/api/public/score-configs',
      'page'
    )) as unknown as ScoreConfig[]
    const existingQueues = (await apiGetAllPages(
      cfg,
      '/api/public/annotation-queues',
      'page'
    )) as unknown as AnnotationQueue[]

    const configsByName = groupByName(existingConfigs)
    const queuesByName = groupByName(existingQueues)

    // --- preflight: validate EVERYTHING before the first POST (zero writes on drift) ---
    const drift: string[] = []

    for (const spec of SCORE_CONFIGS) {
      const matches = configsByName.get(spec.name) ?? []
      if (matches.length > 1) {
        drift.push(
          `score-config '${spec.name}' has ${matches.length} distinct objects (duplicate name)`
        )
        continue
      }
      if (matches.length === 1) {
        const d = scoreConfigDrift(spec, matches[0])
        if (d) drift.push(d)
      }
    }

    const queueMatches = queuesByName.get(TRIAGE_QUEUE_NAME) ?? []
    if (queueMatches.length > 1) {
      drift.push(
        `queue '${TRIAGE_QUEUE_NAME}' has ${queueMatches.length} distinct objects (duplicate name)`
      )
    } else if (queueMatches.length === 1) {
      // The queue can only be validated against the human config ids. If either is
      // absent, an existing queue necessarily references a missing/wrong id → drift.
      const humanIds: string[] = []
      let humanMissing = false
      for (const name of TRIAGE_QUEUE_SCORE_CONFIGS) {
        const c = configsByName.get(name) ?? []
        if (c.length === 1) humanIds.push(c[0].id)
        else humanMissing = true
      }
      if (humanMissing) {
        drift.push(
          `queue '${TRIAGE_QUEUE_NAME}' exists but a required editable score config ` +
            `(${TRIAGE_QUEUE_SCORE_CONFIGS.join(', ')}) is absent/duplicated — cannot reconcile`
        )
      } else if (!sameIdSet(queueMatches[0].scoreConfigIds, humanIds)) {
        drift.push(
          `queue '${TRIAGE_QUEUE_NAME}' scoreConfigIds ${JSON.stringify(queueMatches[0].scoreConfigIds)} ` +
            `!= desired ${JSON.stringify(humanIds)} (${TRIAGE_QUEUE_SCORE_CONFIGS.join(', ')})`
        )
      }
    }

    if (drift.length > 0) {
      errlog('qa-langfuse-setup: DRIFT detected — no changes made. Resolve in the Langfuse UI:')
      for (const d of drift) errlog(`  - ${d}`)
      return 1
    }

    // --- reconcile: create only what's absent (preflight proved matches are clean) ---
    const configIds = await ensureScoreConfigs(cfg, configsByName, log)
    const queueId = await ensureQueue(cfg, queuesByName, configIds, log)

    log('qa-langfuse-setup: done.')
    log(`  queue ${TRIAGE_QUEUE_NAME} = ${queueId}`)
    for (const spec of SCORE_CONFIGS)
      log(`  score-config ${spec.name} = ${configIds.get(spec.name)}`)
    return 0
  } catch (err) {
    if (err instanceof ApiError || err instanceof PaginationError) {
      errlog(`qa-langfuse-setup: ${err.name}: ${err.message}`)
      return 1
    }
    throw err
  }
}

// Create absent configs; matching ones log `exists (matches)`. Returns name→id.
async function ensureScoreConfigs(
  cfg: LangfuseConfig,
  configsByName: Map<string, ScoreConfig[]>,
  log: (msg: string) => void
): Promise<Map<string, string>> {
  const ids = new Map<string, string>()
  for (const spec of SCORE_CONFIGS) {
    const existing = configsByName.get(spec.name)?.[0]
    if (existing) {
      log(`  score-config ${spec.name}: exists (matches)`)
      ids.set(spec.name, existing.id)
      continue
    }
    const created = (await api(cfg, 'POST', '/api/public/score-configs', {
      name: spec.name,
      dataType: spec.dataType,
      categories: spec.categories,
    })) as unknown as ScoreConfig
    log(`  score-config ${spec.name}: created (${created.id})`)
    ids.set(spec.name, created.id)
  }
  return ids
}

// Find-or-create the queue referencing ONLY the human editable dims.
async function ensureQueue(
  cfg: LangfuseConfig,
  queuesByName: Map<string, AnnotationQueue[]>,
  configIds: Map<string, string>,
  log: (msg: string) => void
): Promise<string> {
  const scoreConfigIds = TRIAGE_QUEUE_SCORE_CONFIGS.map(name => {
    const id = configIds.get(name)
    if (!id) throw new Error(`internal: no id for required score config '${name}'`)
    return id
  })
  const existing = queuesByName.get(TRIAGE_QUEUE_NAME)?.[0]
  if (existing) {
    log(`  queue ${TRIAGE_QUEUE_NAME}: exists (matches)`)
    return existing.id
  }
  const created = (await api(cfg, 'POST', '/api/public/annotation-queues', {
    name: TRIAGE_QUEUE_NAME,
    scoreConfigIds,
  })) as unknown as AnnotationQueue
  log(`  queue ${TRIAGE_QUEUE_NAME}: created (${created.id})`)
  return created.id
}

function sameIdSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false
  const sa = new Set(a)
  return b.every(id => sa.has(id))
}

if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main().then(code => {
    process.exitCode = code
  })
}
