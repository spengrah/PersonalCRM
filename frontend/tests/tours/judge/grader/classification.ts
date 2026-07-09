// The load-bearing hybrid-grader decision (design D3): one row per spec
// then-item, keyed by (behavior_id, then_index) EXACTLY as spec/contacts.yaml
// lists them (23 items total across the 7 current contacts `ux` behaviors).
// "Verifiers before judges" — the judge residue is deliberately tiny.
//
// A `verifier` item is graded by pure code over structured evidence. A `judge`
// item is graded by the LLM judge (semantic residue). `judgeFallback` marks a
// verifier item whose success-wording faithfulness MAY route to the judge when
// the verifier cannot bind (CON-043[5]). `caveat` records a capture-coverage
// limitation surfaced in the advisory report (NOT a silent proven-pass).

export type GraderKind = 'verifier' | 'judge'

export interface Classification {
  behaviorId: string
  thenIndex: number
  grader: GraderKind
  /** A verifier item that hands the residue to the judge when it cannot bind. */
  judgeFallback?: boolean
  /** Capture-coverage limitation flagged in the advisory report. */
  caveat?: string
  note: string
}

export const CLASSIFICATION: Classification[] = [
  // --- CON-038: list + detail share one default ordering ---
  {
    behaviorId: 'CON-038',
    thenIndex: 0,
    grader: 'verifier',
    caveat:
      'The tour opens /contacts?sort=cadence&order=desc (explicit), proving cadence-ordering in the default-equivalent context but NOT the implicit no-sort default; a bare-/contacts capture is a tour follow-up.',
    note: 'list defaults to cadence order, most frequent first',
  },
  {
    behaviorId: 'CON-038',
    thenIndex: 1,
    grader: 'verifier',
    note: 'detail prev/next uses the same default ordering (ids_only order == list order)',
  },

  // --- CON-040: keyboard navigation drives the detail page ---
  {
    behaviorId: 'CON-040',
    thenIndex: 0,
    grader: 'verifier',
    caveat:
      'The merged tour captures only the FIRST boundary (Previous disabled); the last boundary (Next disabled at the last contact) is not captured — the verifier abstains on the uncaptured half.',
    note: 'left/right move prev/next, disabled at the boundaries',
  },
  {
    behaviorId: 'CON-040',
    thenIndex: 1,
    grader: 'verifier',
    note: 'arrows inert while editing or focus in an input',
  },
  { behaviorId: 'CON-040', thenIndex: 2, grader: 'verifier', note: 'Enter opens edit mode' },
  {
    behaviorId: 'CON-040',
    thenIndex: 3,
    grader: 'verifier',
    note: 'Escape discards edit, or returns to the list (context preserved)',
  },

  // --- CON-041: action parameters trigger once and are consumed ---
  {
    behaviorId: 'CON-041',
    thenIndex: 0,
    grader: 'verifier',
    note: 'the action runs once (edit opens / merge modal opens)',
  },
  { behaviorId: 'CON-041', thenIndex: 1, grader: 'verifier', note: 'parameter stripped from URL' },

  // --- CON-042: deleting a contact requires explicit confirmation ---
  {
    behaviorId: 'CON-042',
    thenIndex: 0,
    grader: 'judge',
    note: 'confirmation prompt warns the action cannot be undone (the one clearly judge-only item)',
  },
  {
    behaviorId: 'CON-042',
    thenIndex: 1,
    grader: 'verifier',
    note: 'only on confirmation is the contact deleted',
  },
  {
    behaviorId: 'CON-042',
    thenIndex: 2,
    grader: 'verifier',
    note: 'on success returned to the contact list',
  },

  // --- CON-043: the merge flow keeps the current contact, archives the source ---
  {
    behaviorId: 'CON-043',
    thenIndex: 0,
    grader: 'verifier',
    note: 'current marked kept (Keeping badge); pick source from a selector that excludes the target',
  },
  {
    behaviorId: 'CON-043',
    thenIndex: 1,
    grader: 'verifier',
    note: 'selecting a source loads a preview',
  },
  {
    behaviorId: 'CON-043',
    thenIndex: 2,
    grader: 'verifier',
    note: 'conflicting fields toggle, defaulting to keep target',
  },
  {
    behaviorId: 'CON-043',
    thenIndex: 3,
    grader: 'verifier',
    note: 'merged name editable, with source quick-fill',
  },
  {
    behaviorId: 'CON-043',
    thenIndex: 4,
    grader: 'verifier',
    note: 'cannot submit before source / while preview loading / while merge in flight',
  },
  {
    behaviorId: 'CON-043',
    thenIndex: 5,
    grader: 'verifier',
    judgeFallback: true,
    note: 'outcome reported and auto-dismissed (success-wording faithfulness → judge if unbindable)',
  },

  // --- CON-044: mark-as-contacted logs a mutual interaction ---
  {
    behaviorId: 'CON-044',
    thenIndex: 0,
    grader: 'verifier',
    note: 'a mutual-direction interaction is logged, server-timestamped',
  },

  // --- CON-045: the birthdays page groups contacts by proximity ---
  {
    behaviorId: 'CON-045',
    thenIndex: 0,
    grader: 'verifier',
    note: 'grouped into today / upcoming / already-celebrated',
  },
  {
    behaviorId: 'CON-045',
    thenIndex: 1,
    grader: 'verifier',
    note: 'gift-planning section appears only near year end',
  },
  {
    behaviorId: 'CON-045',
    thenIndex: 2,
    grader: 'verifier',
    note: 'upcoming sort soonest-first; celebrated sink to end',
  },
  {
    behaviorId: 'CON-045',
    thenIndex: 3,
    grader: 'verifier',
    note: 'placeholder-year birthdays display without an age',
  },
  {
    behaviorId: 'CON-045',
    thenIndex: 4,
    grader: 'verifier',
    note: 'the page follows accelerated time',
  },
]

// The count MUST match spec/contacts.yaml (CON-038×2, CON-040×4, CON-041×2,
// CON-042×3, CON-043×6, CON-044×1, CON-045×5 = 23) — guarded by a unit test.
export const CLASSIFICATION_ITEM_COUNT = 23

export function classificationFor(behaviorId: string): Classification[] {
  return CLASSIFICATION.filter(c => c.behaviorId === behaviorId).sort(
    (a, b) => a.thenIndex - b.thenIndex
  )
}

export function classificationItem(
  behaviorId: string,
  thenIndex: number
): Classification | undefined {
  return CLASSIFICATION.find(c => c.behaviorId === behaviorId && c.thenIndex === thenIndex)
}
