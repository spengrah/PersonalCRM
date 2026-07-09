// The eval runner (make qa-eval). Two modes:
//   - verifiers-only (DEFAULT): offline, deterministic, the MERGE GATE. Exits
//     non-zero ONLY on a verifier regression (a self-labeled doctored fail not
//     caught, or collateral on a clean case) — never on a missing human label.
//   - --judge: adds the live judge layer over residue items (ADVISORY, manual,
//     needs codex quota). --repeat N measures judge self-consistency.
//
// Loads the JSON corpus (portable JSON.parse). The pure eval logic is core.ts
// (unit tested); this file wires I/O + prints.

import * as path from 'path'
import { loadCorpus } from '../corpus/load'
import { resolveCaseCaptures } from '../doctor'
import { buildJudgeInput } from '../judge-input'
import { behaviorSpec } from '../spec-catalog'
import { selectJudge } from '../adapter'
import { makeJudgeRunner } from '../judge-runner'
import type { Verdict } from '../grader/types'
import { runEval } from './core'
import {
  abstentionRate,
  fmtPct,
  precisionRecall,
  selfConsistency,
  total,
  VERDICTS,
} from './metrics'

function parseArgs(argv: string[]): {
  corpusRoot: string
  judge: boolean
  limit?: number
  repeat: number
} {
  let corpusRoot = path.join(import.meta.dirname ?? __dirname, '..', 'corpus')
  let judge = false
  let limit: number | undefined
  let repeat = 1
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if (a === '--judge') judge = true
    else if (a === '--limit') limit = Number(argv[++i])
    else if (a === '--repeat') repeat = Number(argv[++i])
    else if (!a.startsWith('--')) corpusRoot = a
  }
  return { corpusRoot, judge, limit, repeat }
}

async function main(): Promise<void> {
  const { corpusRoot, judge, limit, repeat } = parseArgs(process.argv.slice(2))
  const corpus = loadCorpus(corpusRoot)
  let cases = corpus.cases
  if (limit !== undefined) cases = cases.slice(0, limit)

  const runner = judge ? makeJudgeRunner() : undefined
  const result = await runEval(cases, c => corpus.capturesFor(c), { judge: runner })

  console.log(
    `\nqa-eval: ${cases.length} case(s), mode=${judge ? 'judge (advisory)' : 'verifiers-only (merge gate)'}\n`
  )

  // Confusion matrix (deterministic classifier).
  console.log('Confusion matrix (expected \\ predicted) — verifier items:')
  console.log(`             ${VERDICTS.map(v => v.padStart(8)).join('')}`)
  for (const e of VERDICTS) {
    console.log(
      `  ${e.padEnd(9)} ${VERDICTS.map(p => String(result.matrix[e][p]).padStart(8)).join('')}`
    )
  }
  console.log('')

  for (const v of VERDICTS) {
    const pr = precisionRecall(result.matrix, v)
    console.log(`  ${v.padEnd(7)} precision=${fmtPct(pr.precision)} recall=${fmtPct(pr.recall)}`)
  }
  console.log(
    `  abstention rate: ${fmtPct(abstentionRate(result.matrix))} (${total(result.matrix)} verifier items)`
  )
  console.log('')

  // Self-consistency (advisory, --judge --repeat).
  if (judge && repeat > 1) {
    const scores: number[] = []
    for (const c of cases) {
      // Doctored cases must repeat against the MUTATED evidence (same resolver
      // the merge-gate path uses), so self-consistency reflects the real inputs.
      const captures = resolveCaseCaptures(c, corpus.capturesFor(c))
      const input = buildJudgeInput(c.behavior_id, captures)
      if (!input || input.items.length === 0) continue
      const j = selectJudge(process.env.QA_JUDGE ?? 'codex-exec')
      const runs: Verdict[][] = []
      for (let r = 0; r < repeat; r++) runs.push((await j(input)).map(v => v.verdict))
      scores.push(selfConsistency(runs))
    }
    const avg = scores.length ? scores.reduce((a, b) => a + b, 0) / scores.length : 1
    console.log(`  judge self-consistency (${repeat} repeats): ${fmtPct(avg)} (advisory)\n`)
  }

  // The JUDGE's actual verdicts on the residue items — the whole point of the
  // agentic layer. Advisory + label-gated, so these are EXCLUDED from the matrix
  // above; printed here so the judgment is visible, not merely counted. These
  // are the candidate critiques a human later corrects into ground-truth labels.
  if (judge) {
    const icon: Record<Verdict, string> = { pass: '✅', fail: '❌', unsure: '⚠️' }
    console.log('Judge verdicts — residue items (advisory; not part of the merge gate):')
    let shown = 0
    for (const c of result.cases) {
      // source==='judge' = every item the judge actually ruled on: pure
      // judge-tagged items AND verifier items that abstained and fell back.
      const judged = c.items.filter(i => i.source === 'judge')
      if (judged.length === 0) continue
      const spec = behaviorSpec(c.behaviorId)
      for (const it of judged) {
        shown += 1
        const thenText = spec?.then[it.thenIndex] ?? ''
        const kind = it.grader === 'verifier' ? ' (verifier-fallback)' : ''
        console.log(`  ${c.caseId} [${it.thenIndex}] ${icon[it.predicted]} ${it.predicted}${kind}`)
        if (thenText) console.log(`      then: ${thenText}`)
        if (it.citation) console.log(`      cite: ${it.citation}`)
        if (it.reason) console.log(`      critique: ${it.reason}`)
      }
    }
    if (shown === 0) console.log('  (none — no judge-tagged residue items in this corpus)')
    console.log('')
  }

  // Label-gated metrics.
  console.log(
    '  fail-precision over held-out (north star): N/A — pending human labels (see judge/DEFERRED.md)'
  )
  console.log(`  judge/residue items pending labels: ${result.judgeItemsPending}\n`)

  // The MERGE GATE.
  if (result.regressions.length > 0) {
    console.error(`qa-eval FAIL: ${result.regressions.length} verifier regression(s):`)
    for (const r of result.regressions) {
      console.error(
        `  ${r.thenIndex}: expected ${r.expected}, predicted ${r.predicted}${r.reason ? ` — ${r.reason}` : ''}`
      )
    }
    process.exit(1)
  }
  console.log(
    'qa-eval PASS: no verifier regression (all self-labeled doctored fails caught, zero collateral).\n'
  )
}

if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main()
}
