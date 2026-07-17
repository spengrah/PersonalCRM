// Single source of truth for the QA triage substrate names + score-config specs.
//
// WHY THIS EXISTS (arc contract #2): the setup script, the exporter (PR2), and the
// backfill CLI (PR3) all reference the SAME triage queue + score names. A name typo
// split across those call sites would silently bind/emit the wrong score or create a
// second queue / drop items. Everything downstream imports these constants — nobody
// spells `'verdict'` / `'qa-triage'` inline.

// The standing annotation queue the judge's fails/traps land in for human triage.
export const TRIAGE_QUEUE_NAME = 'qa-triage'

// The judge's PROGRAMMATIC verdict score. PR2 emits it on the trace by `configId`;
// PR3 reads it back. It is deliberately NOT a queue-editable dimension (see
// TRIAGE_QUEUE_SCORE_CONFIGS) — a second, human-authored `verdict` would overwrite
// the judge's and destroy the judge-vs-human delta the calibration loop measures.
export const VERDICT_SCORE_NAME = 'verdict'

export interface ScoreCategory {
  label: string
  value: number
}

export interface ScoreConfigSpec {
  name: string
  dataType: 'CATEGORICAL'
  categories: ScoreCategory[]
}

// Category VALUE encoding is fixed and load-bearing: pass-ish = 1, unsure = 0,
// fail-ish = -1 (disposition: acted = 1, deferred = 0, dismissed = -1). The API
// requires a numeric value per category, but ANALYSIS reads the string LABEL — so
// the values only need to be stable + documented, which is what this comment is for.
// Changing a value silently reinterprets every score already recorded under it.
export const SCORE_CONFIGS: ScoreConfigSpec[] = [
  {
    name: VERDICT_SCORE_NAME,
    dataType: 'CATEGORICAL',
    categories: [
      { label: 'pass', value: 1 },
      { label: 'unsure', value: 0 },
      { label: 'fail', value: -1 },
    ],
  },
  {
    name: 'ground_truth',
    dataType: 'CATEGORICAL',
    categories: [
      { label: 'should_pass', value: 1 },
      { label: 'unsure', value: 0 },
      { label: 'should_fail', value: -1 },
    ],
  },
  {
    name: 'disposition',
    dataType: 'CATEGORICAL',
    categories: [
      { label: 'acted', value: 1 },
      { label: 'deferred', value: 0 },
      { label: 'dismissed', value: -1 },
    ],
  },
]

// The queue's EDITABLE score dimensions — the two HUMAN dimensions ONLY. `verdict`
// is excluded on purpose (see VERDICT_SCORE_NAME); it still shows read-only on the
// trace in the annotation UI as the judge's call the reviewer is adjudicating.
export const TRIAGE_QUEUE_SCORE_CONFIGS = ['ground_truth', 'disposition'] as const
