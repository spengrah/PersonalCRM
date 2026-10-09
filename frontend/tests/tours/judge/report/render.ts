import { existsSync, readFileSync, readdirSync, writeFileSync } from 'fs'
import path from 'path'
import { DEFAULT_JUDGE_KIND } from '../adapter'
import { allIntents } from '../intent-catalog'
import { makeIntentJudge, runIntentPass, type IntentGrade } from '../intent-runner'
import type { LoadedCapture } from '../../support/run-dir'

function loadCaptures(dir: string): LoadedCapture[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const file = path.join(dir, entry.name)
    if (entry.isDirectory()) return loadCaptures(file)
    if (!entry.name.endsWith('.json')) return []
    const capture = JSON.parse(readFileSync(file, 'utf8')) as LoadedCapture
    capture.__sourceFile = entry.name
    return [capture]
  })
}

export function renderReport(grades: IntentGrade[]): string {
  const lines = ['# UX sanity check', '', 'Advisory report. Review findings before acting.', '']
  const boundGrades = grades.filter(g => g.boundCount > 0)
  for (const grade of boundGrades) {
    lines.push(`## ${grade.intentId} — ${grade.verdict} (${grade.status})`, '', grade.title, '')
    lines.push(
      `Evidence: ${grade.boundCount} captures; ${grade.droppedCount} dropped over the cap.`
    )
    if (grade.ariaOnly)
      lines.push('Visual evidence caveat: screenshots unavailable; judged aria-only.')
    if (grade.citation) lines.push(`Citation: ${grade.citation}`)
    lines.push(`Critique: ${grade.reason ?? 'No critique returned.'}`, '')
  }
  const noEvidence = grades.filter(g => g.boundCount === 0)
  if (noEvidence.length > 0) lines.push('## No evidence', '')
  for (const grade of noEvidence) {
    lines.push(`- ${grade.intentId} (${grade.status}) — ${grade.title}: no bound evidence.`)
  }
  return lines.join('\n') + '\n'
}

export function allBoundGradesAreJudgeErrors(grades: IntentGrade[]): boolean {
  const boundGrades = grades.filter(grade => grade.boundCount > 0)
  return boundGrades.length > 0 && boundGrades.every(grade => grade.judgeError === true)
}

export async function main(argv = process.argv.slice(2)): Promise<boolean> {
  const [runDir, output] = argv
  if (!runDir || argv.length > 2) throw new Error('usage: render.ts <runDir> [outFile]')
  const captures = loadCaptures(path.join(runDir, 'captures'))
  const kind = process.env.QA_JUDGE ?? DEFAULT_JUDGE_KIND
  const resolveScreenshot = (capture: LoadedCapture): string | undefined => {
    if (!capture.screenshot) return undefined
    const file = path.resolve(runDir, capture.screenshot)
    return existsSync(file) ? file : undefined
  }
  const grades = await runIntentPass(
    captures,
    makeIntentJudge(kind),
    allIntents(),
    undefined,
    kind === 'http' ? undefined : resolveScreenshot
  )
  const file = path.resolve(output ?? path.join(runDir, 'sanity-report.md'))
  writeFileSync(file, renderReport(grades), 'utf8')
  console.log(file)
  return allBoundGradesAreJudgeErrors(grades)
}

if (import.meta.main) {
  main().catch(error => {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  })
}
