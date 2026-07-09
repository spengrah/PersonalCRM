// The advisory run report (markdown). A per-behavior/-item verdict roll-up over
// a run's captures — every fail human-reviewable. This arc is ADVISORY: the
// report FILES NO ISSUES (issue-mode is the deferred PR4). Label-gated metrics
// (held-out fail-precision, error-analysis taxonomy) print N/A.
//
// Pure `renderReport` (testable) + a bun CLI that loads a .runs run dir.

import { CLASSIFICATION } from '../grader/classification'
import { SPEC_CATALOG } from '../spec-catalog'
import { SKIP_LIST } from './skip-list'
import type { BehaviorGrade } from '../grader/grade'
import type { Verdict } from '../grader/types'

export interface ReportMeta {
  runId?: string
  gitSha?: string
  generatedAt?: string
}

export interface ReportInput {
  meta?: ReportMeta
  grades: BehaviorGrade[]
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
  lines.push('> ADVISORY ONLY — this report files NO issues (issue-mode is the deferred PR4).')
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
      const src =
        item.source === 'verifier'
          ? 'verifier'
          : item.source === 'judge'
            ? 'judge'
            : 'judge (pending labels)'
      const cite = item.citation ? ` — cite: ${item.citation}` : ''
      const reason = item.reason ? ` — ${item.reason}` : ''
      lines.push(
        `- [${item.thenIndex}] ${ICON[item.verdict]} **${item.verdict}** (${src})${cite}${reason}`
      )
    }
    lines.push('')
  }

  // Coverage — first-cut scope (D5): the 3 scoped domains' current-ux behaviors
  // (toured vs untoured) + the explicit skip-list. Advisory; files no issues; NOT
  // a repo-wide scanner (the other SSOT domains are Piece 3's scope).
  lines.push(...renderCoverage(grades))

  // Capture-coverage caveats (honest limitations, NOT graded passes).
  const caveats = CLASSIFICATION.filter(c => c.caveat)
  if (caveats.length > 0) {
    lines.push('## Capture-coverage caveats (untoured / partial — tour follow-ups)')
    lines.push('')
    for (const c of caveats) {
      lines.push(`- **${c.behaviorId}[${c.thenIndex}]**: ${c.caveat}`)
    }
    lines.push('')
  }

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
  const [runDir, outFile] = process.argv.slice(2)
  if (!runDir) {
    console.error('usage: render.ts <runDir> [outFile]')
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
  const captures = files.map(f => JSON.parse(fs.readFileSync(f, 'utf8')))
  const grades = groupByBehavior(captures).map(set => gradeBehavior(set))
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
  const md = renderReport({ meta: { runId, gitSha }, grades })
  if (outFile) fs.writeFileSync(outFile, md, 'utf8')
  else process.stdout.write(md)
}

if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main()
}
