export type Verdict = 'pass' | 'fail' | 'unsure'

export interface ItemVerdict {
  verdict: Verdict
  citation?: string
  reason?: string
}

function hasCitation(v: ItemVerdict): boolean {
  return typeof v.citation === 'string' && v.citation.trim() !== ''
}

// Grounding rule (D4): a `fail` with no resolvable citation is downgraded to
// `unsure` — applied to the judge's verdicts after parsing.
export function applyGrounding(v: ItemVerdict): ItemVerdict {
  if (v.verdict === 'fail' && !hasCitation(v)) {
    return {
      verdict: 'unsure',
      reason: `${v.reason ?? 'fail'} — downgraded to unsure: no grounding citation`,
    }
  }
  return v
}
