// The advisory run report (markdown). A per-behavior/-item verdict roll-up over
// a run's captures — every fail human-reviewable. The report is ADVISORY and
// files NO issues. Label-gated metrics (held-out fail-precision, error-analysis
// taxonomy) print N/A.
//
// Pure `renderReport` (testable) + a bun CLI that loads a .runs run dir.

import { SPEC_CATALOG } from '../spec-catalog'
import { SKIP_LIST } from './skip-list'
import type { BehaviorGrade } from '../grader/grade'
import type { Verdict } from '../grader/types'
import type { IntentGrade } from '../intent-runner'

export interface ReportMeta {
  runId?: string
  gitSha?: string
  generatedAt?: string
}

export interface ReportInput {
  meta?: ReportMeta
  grades: BehaviorGrade[]
  // Intent-pass grades (present only on --judge runs; the pass costs quota).
  intents?: IntentGrade[]
}

const ICON: Record<Verdict, string> = { pass: '✅', fail: '❌', unsure: '⚠️' }

function countByVerdict(grade: BehaviorGrade): Record<Verdict, number> {
  const c: Record<Verdict, number> = { pass: 0, fail: 0, unsure: 0 }
  for (const i of grade.items) c[i.verdict] += 1
  return c
}

// The 3 first-cut domains, keyed by behavior-id prefix. The current-ux behavior
// set is the spec-catalog (hand-transcribed, completeness-guarded by a test).
const DOMAINS: { name: string; prefix: string }[] = [
  { name: 'contacts', prefix: 'CON-' },
  { name: 'dashboard', prefix: 'DSH-' },
  { name: 'cadence-followup', prefix: 'CAD-' },
]

function renderCoverage(grades: BehaviorGrade[]): string[] {
  const toured = new Set(grades.map(g => g.behaviorId))
  const catalogIds = Object.keys(SPEC_CATALOG).sort()
  const lines: string[] = []
  lines.push('## Coverage — first-cut scope')
  lines.push('')
  lines.push(
    '> Scoped to the 3 first-cut domains (contacts, dashboard, cadence-followup). The other SSOT ' +
      'domains are out of scope here (Piece 3 owns the repo-wide scanner). Advisory — files no issues.'
  )
  lines.push('')
  for (const { name, prefix } of DOMAINS) {
    const ids = catalogIds.filter(id => id.startsWith(prefix))
    if (ids.length === 0) continue
    lines.push(`### ${name}`)
    lines.push('')
    for (const id of ids) {
      const isToured = toured.has(id)
      lines.push(
        `- ${isToured ? '✅ toured' : '⬜ untoured'} — **${id}** ${SPEC_CATALOG[id].title}`
      )
    }
    lines.push('')
  }
  lines.push('### Skip-list (untoured behaviors / clauses, with reasons)')
  lines.push('')
  for (const s of SKIP_LIST) {
    lines.push(`- **${s.id}** — ${s.reason}`)
  }
  lines.push('')
  return lines
}

export function renderReport(input: ReportInput): string {
  const { grades } = input
  const meta = input.meta ?? {}
  const lines: string[] = []

  lines.push('# Agentic UX QA — advisory run report')
  lines.push('')
  lines.push('> ADVISORY ONLY — this report files NO issues.')
  lines.push(
    '> Every `fail` below is human-reviewable; `unsure` is abstention (never issue-eligible).'
  )
  lines.push('')
  if (meta.runId) lines.push(`- run: \`${meta.runId}\``)
  if (meta.gitSha) lines.push(`- gitSha: \`${meta.gitSha}\``)
  lines.push(`- generated: ${meta.generatedAt ?? new Date().toISOString()}`)
  lines.push('')

  // Roll-up table.
  lines.push('## Behavior roll-up')
  lines.push('')
  lines.push('| Behavior | Verdict | pass | fail | unsure |')
  lines.push('|---|---|---|---|---|')
  for (const g of grades) {
    const c = countByVerdict(g)
    lines.push(
      `| ${g.behaviorId} | ${ICON[g.behaviorVerdict]} ${g.behaviorVerdict} | ${c.pass} | ${c.fail} | ${c.unsure} |`
    )
  }
  lines.push('')

  // Per-behavior detail.
  lines.push('## Per-behavior detail')
  lines.push('')
  for (const g of grades) {
    lines.push(`### ${g.behaviorId} — ${ICON[g.behaviorVerdict]} ${g.behaviorVerdict}`)
    lines.push('')
    for (const item of g.items) {
      const src = item.source === 'judge' ? 'judge' : 'judge (pending labels)'
      const cite = item.citation ? ` — cite: ${item.citation}` : ''
      const reason = item.reason ? ` — ${item.reason}` : ''
      lines.push(
        `- [${item.thenIndex}] ${ICON[item.verdict]} **${item.verdict}** (${src})${cite}${reason}`
      )
    }
    lines.push('')
  }

  // Intents — the judged experience goals (type: intent in the SSOT). A current
  // intent failing is a REGRESSION signal; a proposed intent passing is a
  // PROGRESS signal (candidate to flip current). Advisory, judge-only.
  if (input.intents && input.intents.length > 0) {
    lines.push('## Intents — judged experience goals (advisory)')
    lines.push('')
    lines.push(
      '> One judge call per intent over the captures bound via its `serves:` edges. ' +
        'A `current` intent failing is a regression signal; a `proposed` intent passing is a ' +
        'progress signal (consider flipping it current in the SSOT).'
    )
    lines.push('')
    for (const g of input.intents) {
      const signal =
        g.status === 'current'
          ? g.verdict === 'fail'
            ? ' — ⚠️ REGRESSION SIGNAL'
            : ''
          : g.verdict === 'pass'
            ? ' — 📈 progress signal (proposed goal judged achieved)'
            : ' (proposed)'
      lines.push(`### ${g.intentId} — ${ICON[g.verdict]} ${g.verdict}${signal}`)
      lines.push('')
      lines.push(`- ${g.title}`)
      lines.push(
        `- evidence: ${g.boundCount} capture(s) via serves-edges [${g.servedBy.join(', ')}]` +
          (g.droppedCount > 0 ? ` — ${g.droppedCount} over the cap DROPPED (not judged)` : '')
      )
      if (g.ariaOnly) {
        lines.push(
          '- ⚠️ EVIDENCE CAVEAT: visual intent judged aria-only (no screenshots attached) — the verdict cannot observe layout/hierarchy/salience'
        )
      }
      if (g.citation) lines.push(`- cite: ${g.citation}`)
      if (g.reason) lines.push(`- critique: ${g.reason}`)
      lines.push('')
    }
  }

  // Coverage — first-cut scope (D5): the 3 scoped domains' current-ux behaviors
  // (toured vs untoured) + the explicit skip-list. Advisory; files no issues; NOT
  // a repo-wide scanner (the other SSOT domains are Piece 3's scope).
  lines.push(...renderCoverage(grades))

  // Deferred / label-gated metrics.
  lines.push('## Deferred metrics (label-gated)')
  lines.push('')
  lines.push(
    '- fail-precision over a labeled held-out set: **N/A — pending human labels** (see judge/DEFERRED.md)'
  )
  lines.push(
    '- error-analysis-first failure taxonomy over real captures: **N/A — pending human labels** (see judge/DEFERRED.md)'
  )
  lines.push(
    '- judge-layer precision/recall vs human ground truth: **N/A — pending human labels** (see judge/DEFERRED.md)'
  )
  lines.push('')

  return lines.join('\n')
}

// CLI (bun): bun run tests/tours/judge/report/render.ts <runDir> [outFile]
async function main(): Promise<void> {
  const fs = await import('fs')
  const path = await import('path')
  const { groupByBehavior, gradeBehavior } = await import('../grader/grade')
  const argv = process.argv.slice(2)
  if (argv.includes('-h') || argv.includes('--help')) {
    console.log('usage: render.ts <runDir> [outFile] [--judge]')
    console.log(
      '  --judge  run the LLM judge over residue items (advisory; needs codex quota / QA_JUDGE)'
    )
    process.exit(0)
  }
  const useJudge = argv.includes('--judge')
  const [runDir, outFile] = argv.filter(a => !a.startsWith('--'))
  if (!runDir) {
    console.error('usage: render.ts <runDir> [outFile] [--judge]')
    process.exit(2)
  }
  const capturesRoot = path.join(runDir, 'captures')
  const files: string[] = []
  const walk = (d: string): void => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const full = path.join(d, e.name)
      if (e.isDirectory()) walk(full)
      else if (full.endsWith('.json')) files.push(full)
    }
  }
  walk(capturesRoot)
  // Stamp each capture with its REAL source filename (LOADER-ONLY, off the
  // persisted `Capture` format) so the label-trace export can attribute graded
  // evidence to its file (spec line 53). `captureSection` copies it onto
  // `CaptureSection.captureFile` before the info is lost.
  type LoadedCapture = import('../../support/run-dir').LoadedCapture
  const captures = files.map(f => {
    const c = JSON.parse(fs.readFileSync(f, 'utf8')) as LoadedCapture
    c.__sourceFile = path.basename(f)
    return c
  })

  // The advisory judge layer over residue items (opt-in; the report is advisory
  // either way). Without --judge, judge-tagged items render as "pending labels";
  // with it, the judge's grounded verdict + critique lands in the per-item detail.
  // Capability gate shared by the item-judge and intent passes: only the
  // codex-exec adapter can attach image files.
  const canAttachImages = (process.env.QA_JUDGE ?? 'codex-exec') === 'codex-exec'
  const resolveScreenshot = (c: { screenshot?: string }): string | undefined => {
    if (!c.screenshot) return undefined
    const abs = path.resolve(runDir, c.screenshot)
    return fs.existsSync(abs) ? abs : undefined
  }
  const runner = useJudge
    ? (await import('../judge-runner')).makeJudgeRunner(
        undefined,
        canAttachImages ? resolveScreenshot : undefined
      )
    : undefined
  // Grade SERIALLY. The judge calls must NOT fan out: concurrent codex spawns
  // storm a quota-limited account, so keep one codex subprocess at a time.
  const grades = []
  for (const set of groupByBehavior(captures)) {
    const judge = runner ? await runner(set.behaviorId, set.captures) : undefined
    grades.push(gradeBehavior(set, judge ? { judge } : {}))
  }

  // The intent pass (judge-only; serial like the residue path). Uses its own
  // stronger-model default (QA_INTENT_MODEL / QA_INTENT_EFFORT). Screenshots
  // recorded by the tours attach as model images when the file exists in the
  // run dir (live evidence only — the committed corpus stays aria-only).
  let intents
  if (useJudge) {
    const { makeIntentJudge, runIntentPass } = await import('../intent-runner')
    const { INTENT_CAPTURE_CAP } = await import('../intent-input')
    const { allIntents } = await import('../intent-catalog')
    intents = await runIntentPass(
      captures,
      makeIntentJudge(),
      allIntents(),
      INTENT_CAPTURE_CAP,
      canAttachImages ? resolveScreenshot : undefined
    )
  }
  let runId: string | undefined
  let gitSha: string | undefined
  const manifestPath = path.join(runDir, 'manifest.json')
  if (fs.existsSync(manifestPath)) {
    const m = JSON.parse(fs.readFileSync(manifestPath, 'utf8')) as {
      runId?: string
      gitSha?: string
    }
    runId = m.runId
    gitSha = m.gitSha
  }
  const md = renderReport({ meta: { runId, gitSha }, grades, intents })
  if (outFile) fs.writeFileSync(outFile, md, 'utf8')
  else process.stdout.write(md)
}

if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main()
}
