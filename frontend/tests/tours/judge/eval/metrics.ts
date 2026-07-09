// Eval metrics: a confusion matrix over the deterministic classifier, per-verdict
// precision/recall, abstention rate, and judge self-consistency. Pure.

export type Verdict = 'pass' | 'fail' | 'unsure'
export const VERDICTS: Verdict[] = ['pass', 'fail', 'unsure']

// matrix[expected][predicted] = count.
export type ConfusionMatrix = Record<Verdict, Record<Verdict, number>>

export function emptyMatrix(): ConfusionMatrix {
  const row = (): Record<Verdict, number> => ({ pass: 0, fail: 0, unsure: 0 })
  return { pass: row(), fail: row(), unsure: row() }
}

export function addToMatrix(m: ConfusionMatrix, expected: Verdict, predicted: Verdict): void {
  m[expected][predicted] += 1
}

export function total(m: ConfusionMatrix): number {
  let n = 0
  for (const e of VERDICTS) for (const p of VERDICTS) n += m[e][p]
  return n
}

export interface PrecisionRecall {
  precision: number | null
  recall: number | null
}

export function precisionRecall(m: ConfusionMatrix, verdict: Verdict): PrecisionRecall {
  const tp = m[verdict][verdict]
  let predictedTotal = 0
  let expectedTotal = 0
  for (const v of VERDICTS) {
    predictedTotal += m[v][verdict]
    expectedTotal += m[verdict][v]
  }
  return {
    precision: predictedTotal === 0 ? null : tp / predictedTotal,
    recall: expectedTotal === 0 ? null : tp / expectedTotal,
  }
}

// Fraction of items PREDICTED unsure (abstention).
export function abstentionRate(m: ConfusionMatrix): number {
  const t = total(m)
  if (t === 0) return 0
  let abstained = 0
  for (const e of VERDICTS) abstained += m[e].unsure
  return abstained / t
}

// Judge self-consistency: over N repeat runs of the SAME inputs (each an array
// of per-item verdicts, aligned by index), the fraction of items whose verdict
// was identical across every run. 1 = perfectly stable.
export function selfConsistency(runs: Verdict[][]): number {
  if (runs.length < 2) return 1
  const items = runs[0].length
  if (items === 0) return 1
  let stable = 0
  for (let i = 0; i < items; i++) {
    const first = runs[0][i]
    if (runs.every(r => r[i] === first)) stable += 1
  }
  return stable / items
}

export function fmtPct(x: number | null): string {
  return x === null ? 'n/a' : `${(x * 100).toFixed(1)}%`
}
