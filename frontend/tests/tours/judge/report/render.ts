// The advisory run report (markdown). A per-behavior/-item verdict roll-up over
// a run's captures — every fail human-reviewable. The report is ADVISORY and
// files NO issues. Label-gated metrics (held-out fail-precision, error-analysis
// taxonomy) print N/A.
//
// Pure `renderReport` (testable) + a bun CLI that loads a .runs run dir.

import { SPEC_CATALOG } from '../spec-catalog'
import { SKIP_LIST } from './skip-list'
import { gradeBehavior, groupByBehavior, type BehaviorGrade } from '../grader/grade'
import type { ItemVerdicts, Verdict } from '../grader/types'
import { runIntentPass, type IntentGrade } from '../intent-runner'
import { allIntents } from '../intent-catalog'
import { INTENT_CAPTURE_CAP, type ScreenshotResolver } from '../intent-input'
import { runTrapSelfTest, selftestExitCode, type TrapResult } from '../trap-selftest'
import type { TrapSpec } from '../trap-config'
import type { JudgeRunner } from '../judge-runner'
import { DEFAULT_JUDGE_KIND, type Judge } from '../adapter'
import type { Capture } from '../../support/types'

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
  // Trap self-test results (present only on --judge runs). Undefined renders NO
  // self-test section — a bare (non-judge) report, never a passing self-test.
  trapResults?: TrapResult[]
  // Judge-lane exceptions (residue/intent) caught during the round — a harness
  // failure, surfaced visibly and forcing a non-zero exit. Undefined = none.
  laneErrors?: string[]
}

const ICON: Record<Verdict, string> = { pass: '✅', fail: '❌', unsure: '⚠️' }

// The self-test lane's own status icons — deliberately distinct from the
// advisory verdict icons so the two lanes never read as one.
const TRAP_ICON: Record<TrapResult['status'], string> = {
  caught: '✅',
  missed: '❌',
  error: '🔴',
}

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

  // Judge-lane errors — a residue/intent judge threw during the round. The
  // report is still written (so the failure is diagnosable) and the exit is
  // non-zero. This is a HARNESS failure, not an advisory app verdict.
  if (input.laneErrors && input.laneErrors.length > 0) {
    lines.push('## Judge-lane errors (harness failure — forces non-zero exit)')
    lines.push('')
    for (const e of input.laneErrors) lines.push(`- 🔴 ${e}`)
    lines.push('')
  }

  // Detection self-test (traps) — tests the JUDGE (the detector), not the app.
  // Each trap doctors this round's own captures and MUST be caught (a grounded
  // fail); a missed / non-executable / errored trap drives a non-zero exit — a
  // HARD signal, distinct from the advisory verdicts above (which never gate).
  // Rendered only on judged rounds; a bare report omits it entirely.
  if (input.trapResults) {
    lines.push('## Detection self-test (traps)')
    lines.push('')
    lines.push(
      '> Tests the JUDGE (the detector), NOT the app. Each trap applies a committed mutation to ' +
        "this round's own captures and MUST be caught (a grounded `fail`). A missed / " +
        'non-executable / errored trap sets a non-zero exit — a hard signal separate from the ' +
        'advisory verdicts above.'
    )
    lines.push('')
    for (const t of input.trapResults) {
      lines.push(
        `- ${TRAP_ICON[t.status]} **${t.status}** — \`${t.id}\` (${t.targetBehavior}[${t.targetItem}]) — ${t.reason}`
      )
    }
    lines.push('')
  }

  // Coverage — first-cut scope (D5): the 3 scoped domains' current-ux behaviors
  // (toured vs untoured) + the explicit skip-list. Advisory; files no issues; NOT
  // a repo-wide scanner (the other SSOT domains are Piece 3's scope).
  lines.push(...renderCoverage(grades))

  // Deferred / label-gated metrics.
  lines.push('## Deferred metrics (label-gated)')
  lines.push('')
  lines.push(
    '- fail-precision over a labeled held-out set: **N/A — pending human labels** (see .ai/spec/2026-07-19-codex-sdk-judge-transport.md)'
  )
  lines.push(
    '- error-analysis-first failure taxonomy over real captures: **N/A — pending human labels** (see .ai/spec/2026-07-19-codex-sdk-judge-transport.md)'
  )
  lines.push(
    '- judge-layer precision/recall vs human ground truth: **N/A — pending human labels** (see .ai/spec/2026-07-19-codex-sdk-judge-transport.md)'
  )
  lines.push('')

  return lines.join('\n')
}

// The judge dependencies a JUDGED round needs — an all-or-nothing bundle. Its
// presence IS the `--judge` opt-in: `undefined` = today's bare report (pending
// labels, no intent pass, no traps, zero judge calls, exit 0). Every dep is
// INJECTED, so `runJudgeRound` never consults env config or spawns an adapter —
// tests pass mocks; production builds them from `selectJudge` under `--judge`.
// Which judge adapters can attach screenshots as model images: the codex
// adapters can (codex-exec via `-i`, codex-sdk via local_image entries); the
// text-only http stub cannot. Pure; the CLI gate and its regression test both
// key off this so enabling a new adapter can't silently drop visual grounding.
export function canAttachImagesFor(kind: string): boolean {
  return kind === 'codex-exec' || kind === 'codex-sdk'
}

export interface JudgesBundle {
  // Residue grading: one call per behavior with judge-tagged residue items.
  residueRunner: JudgeRunner
  // The intent pass judge (its own stronger-model default).
  intentJudge: Judge
  // The RAW judge the trap self-test calls directly (so `__trap` reaches the
  // adapter — the runner would rebuild its input and drop it).
  trapJudge: Judge
  // The committed traps to apply this round.
  traps: TrapSpec[]
  resolveScreenshot?: ScreenshotResolver
}

export interface JudgeRoundResult {
  markdown: string
  trapResults: TrapResult[]
  exitCode: 0 | 1
}

// Orchestrate one report round over already-loaded captures. Exported + purely
// dependency-injected so the hard-exit path is unit-testable without a CLI spawn.
// With `judges`: residue grading + intent pass + trap self-test, exit driven by
// the traps. Without it: the bare advisory report, exit 0.
//
// CONTRACT (INV-5): the report is ALWAYS produced — a judge-lane exception
// (residue, intent, OR trap) NEVER rejects this function. Every lane is guarded;
// an exception becomes a visible "judge-lane error" note AND forces exitCode 1
// (a harness that could not run a lane cannot certify the round). The trap lane
// self-guards each trap into an `error` TrapResult; residue/intent throws are
// caught here. This keeps the report writable so a reviewer can diagnose the
// failure, while the non-zero exit still surfaces it.
export async function runJudgeRound(
  captures: Capture[],
  opts: { meta?: ReportMeta; judges?: JudgesBundle } = {}
): Promise<JudgeRoundResult> {
  const { meta, judges } = opts
  const laneErrors: string[] = []
  const errMsg = (e: unknown): string => (e instanceof Error ? e.message : String(e))

  // Grade SERIALLY. The judge calls must NOT fan out: concurrent codex spawns
  // storm a quota-limited account, so keep one codex subprocess at a time. A
  // residue-judge throw for one behavior degrades THAT behavior to pending (a
  // visible lane error) without aborting the round.
  const grades: BehaviorGrade[] = []
  for (const set of groupByBehavior(captures)) {
    let judge: ItemVerdicts | undefined
    if (judges) {
      try {
        judge = await judges.residueRunner(set.behaviorId, set.captures)
      } catch (e) {
        laneErrors.push(`residue judge for ${set.behaviorId}: ${errMsg(e)}`)
      }
    }
    grades.push(gradeBehavior(set, judge ? { judge } : {}))
  }

  // The intent pass + trap self-test ride the JUDGED round only. The intent pass
  // attaches tour screenshots as model images (when the file exists); the trap
  // pass does NOT (undoctorable pixels would defeat the doctoring — see below).
  // The trap pass calls the RAW judge with its own doctored input so the `__trap`
  // marker traverses to the adapter's span.
  let intents: IntentGrade[] | undefined
  let trapResults: TrapResult[] = []
  if (judges) {
    try {
      intents = await runIntentPass(
        captures,
        judges.intentJudge,
        allIntents(),
        INTENT_CAPTURE_CAP,
        judges.resolveScreenshot
      )
    } catch (e) {
      // An intent-pass throw does not abort the round — the trap self-test still
      // runs and the report is still written.
      laneErrors.push(`intent pass: ${errMsg(e)}`)
    }
    // No resolveScreenshot: the trap self-test judges doctored STRUCTURED
    // evidence only — screenshots are undoctorable, so attaching them lets the
    // judge read the truth off the pixels and bypass the doctoring (see
    // trap-selftest.ts). The residue + intent passes above DO get screenshots.
    trapResults = await runTrapSelfTest(captures, judges.traps, judges.trapJudge)
  }

  const markdown = renderReport({
    meta,
    grades,
    intents,
    // Only a judged round renders the self-test section; a bare round passes
    // undefined so the section is omitted entirely.
    trapResults: judges ? trapResults : undefined,
    laneErrors: laneErrors.length > 0 ? laneErrors : undefined,
  })
  // A missed/errored trap OR any judge-lane exception forces exit 1 — the round
  // could not be certified. (Advisory app verdicts, per D7, never gate; this is
  // a HARNESS-failure signal, not an app verdict.)
  const exitCode: 0 | 1 = selftestExitCode(trapResults) === 1 || laneErrors.length > 0 ? 1 : 0
  return { markdown, trapResults, exitCode }
}

// CLI (bun): bun run tests/tours/judge/report/render.ts <runDir> [outFile]
// Exported so its exit behavior is unit-testable. It NEVER calls process.exit()
// — every path sets the DEFERRED `process.exitCode` and returns, so the process
// finishes naturally (flushing the report + the QA_JUDGE_TRACE JSONL) before the
// status is read.
export async function main(): Promise<void> {
  const fs = await import('fs')
  const path = await import('path')
  const argv = process.argv.slice(2)
  if (argv.includes('-h') || argv.includes('--help')) {
    console.log('usage: render.ts <runDir> [outFile] [--judge]')
    console.log(
      '  --judge  run the LLM judge over residue items (advisory; needs codex quota / QA_JUDGE)'
    )
    process.exitCode = 0
    return
  }
  const useJudge = argv.includes('--judge')
  const [runDir, outFile] = argv.filter(a => !a.startsWith('--'))
  if (!runDir) {
    console.error('usage: render.ts <runDir> [outFile] [--judge]')
    process.exitCode = 2
    return
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
  // Capability gate shared by the item-judge and intent passes (see
  // canAttachImagesFor).
  const canAttachImages = canAttachImagesFor(process.env.QA_JUDGE ?? DEFAULT_JUDGE_KIND)
  const resolveScreenshot = (c: { screenshot?: string }): string | undefined => {
    if (!c.screenshot) return undefined
    const abs = path.resolve(runDir, c.screenshot)
    return fs.existsSync(abs) ? abs : undefined
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

  // Build the judge deps ONLY under --judge (the opt-in boundary). ONE raw judge
  // powers both the residue runner AND the trap self-test — the trap pass needs
  // the RAW judge so its `__trap` marker traverses to the adapter (the runner
  // would rebuild its input and drop it); residue reuses it via runnerFromJudge.
  // The intent pass uses its own stronger-model default. Without --judge,
  // `judges` stays undefined → the bare report, zero judge calls, exit 0.
  let judges: JudgesBundle | undefined
  if (useJudge) {
    const { selectJudge } = await import('../adapter')
    const { runnerFromJudge } = await import('../judge-runner')
    const { makeIntentJudge } = await import('../intent-runner')
    const { TRAPS } = await import('../trap-config')
    const raw = selectJudge()
    const rs = canAttachImages ? resolveScreenshot : undefined
    judges = {
      residueRunner: runnerFromJudge(raw, rs),
      trapJudge: raw,
      intentJudge: makeIntentJudge(),
      traps: TRAPS,
      resolveScreenshot: rs,
    }
  }

  const { markdown, exitCode } = await runJudgeRound(captures, {
    meta: { runId, gitSha },
    judges,
  })
  if (outFile) fs.writeFileSync(outFile, markdown, 'utf8')
  else process.stdout.write(markdown)
  // Deferred (NEVER process.exit()): the process finishes naturally so the
  // report + the QA_JUDGE_TRACE JSONL fully flush BEFORE the hard status is
  // read. A missed / non-executable / errored trap sets 1; the operator exports
  // the doctored trace as an INDEPENDENT step (see the Makefile runbook).
  process.exitCode = exitCode
}

if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main()
}
