// The hybrid-grader classification (design D3): rows keyed by
// (behavior_id, then_index) EXACTLY as the spec YAMLs list them. Originally one
// row per then-item; the CON contacts, DSH dashboard, and CAD cadence-followup
// behaviors have all migrated to E2E, so this is now an index-faithful SUBSET —
// the only remaining rows are the CON-042[0] + DSH-004[2] judge items. The
// subset is guarded by grade.test.ts's EXPECTED_ROWS (INV-1), not a total-count
// check. "Verifiers before judges" — the judge residue is deliberately tiny.
//
// A `verifier` item is graded by pure code over structured evidence. A `judge`
// item is graded by the LLM judge (semantic residue). A verifier that cannot
// bind a BINDING-VEHICLE anchor (copy incidental to the asserted fact) emits
// `unbound`, which routes that item to the judge at grade time — the dynamic
// replacement for the old static judgeFallback flag. PRESENCE-CONTRACT items
// keep deterministic fail-on-missing (see grader/types.ts for the policy).
// `caveat` records a capture-coverage limitation surfaced in the advisory
// report (NOT a silent proven-pass).

export type GraderKind = 'verifier' | 'judge'

export interface Classification {
  behaviorId: string
  thenIndex: number
  grader: GraderKind
  /** Capture-coverage limitation flagged in the advisory report. */
  caveat?: string
  note: string
}

export const CLASSIFICATION: Classification[] = [
  // CON-038 (list + detail share one default ordering) migrated to E2E:
  // contacts.spec.ts + contact-navigation.spec.ts (see `// spec: CON-038`).

  // CON-040 (keyboard navigation drives the detail page) migrated to E2E:
  // contact-navigation.spec.ts (see `// spec: CON-040`).

  // CON-041 (action parameters trigger once and are consumed) migrated to E2E:
  // contacts.spec.ts (see `// spec: CON-041`).

  // --- CON-042: deleting a contact requires explicit confirmation ---
  // [1] (only on confirmation is the contact deleted) and [2] (on success
  // returned to the list) migrated to E2E: contacts.spec.ts (see
  // `// spec: CON-042`). [0] stays — the judge owns the "cannot be undone"
  // warning (the one clearly judge-only item).
  {
    behaviorId: 'CON-042',
    thenIndex: 0,
    grader: 'judge',
    note: 'confirmation prompt warns the action cannot be undone (the one clearly judge-only item)',
  },

  // CON-043 (merge flow keeps the current contact, archives the source)
  // migrated to E2E: contact-merge.spec.ts (see `// spec: CON-043`).

  // CON-044 (mark-as-contacted logs a mutual interaction) migrated to E2E:
  // contacts.spec.ts (see `// spec: CON-044`).

  // CON-045 (the birthdays page groups contacts by proximity) migrated to E2E:
  // birthdays.spec.ts (see `// spec: CON-045`).

  // DSH-001 (the dashboard is the default landing surface) migrated to E2E:
  // dashboard.spec.ts (see `// spec: DSH-001`).

  // DSH-002 (persistent global navigation) migrated to E2E:
  // navigation.spec.ts (see `// spec: DSH-002`).

  // DSH-003 (always a path to add or browse contacts) migrated to E2E:
  // dashboard.spec.ts + error-boundary.spec.ts — the header CTA is asserted in
  // all four widget states (see `// spec: DSH-003`).

  // --- DSH-004: loading + error states distinct from content ---
  // [0] (loading → placeholder) and [1] (failure → error state with a reason)
  // migrated to E2E: error-boundary.spec.ts (see `// spec: DSH-004`). [2]
  // stays — the judge owns reason-faithfulness. thenIndex is spec-faithful and
  // never re-indexed, so the surviving judge row keeps its gapped index 2.
  {
    behaviorId: 'DSH-004',
    thenIndex: 2,
    grader: 'judge',
    note: 'the shown failure reason faithfully reflects the actual failure (judge-primary; was a judgeFallback residue on the old combined item)',
  },

  // DSH-005 (overdue widget reflects out-of-flow membership changes):
  // [0] (refreshes without a reload — the deterministically tourable
  // on-dashboard interaction:created trigger) migrated to E2E:
  // dashboard.spec.ts (see `// spec: DSH-005`). [1] (merge / meeting-note
  // trigger breadth), [2] (cosmetic-edit no-op), and [3] (refocus/staleTime
  // timing) were verifier-ABSTAINED (never deterministically provable from a
  // tour) and are deleted WITHOUT an E2E replacement — item-level coverage
  // intentionally dropped; the behavior remains speakable holistically under
  // its DSH-012 intent.

  // DSH-007 (search is the contact list's; no global search) migrated to E2E:
  // contacts.spec.ts ([0]) + dashboard.spec.ts ([1]) (see `// spec: DSH-007`).

  // CAD-026 (dashboard action-required overdue list) migrated to E2E:
  // dashboard.spec.ts (see `// spec: CAD-026`).

  // CAD-027 (urgency / name / recency orderings) migrated to E2E:
  // dashboard.spec.ts (see `// spec: CAD-027`).

  // CAD-028 (mark-contacted from the dashboard) migrated to E2E:
  // dashboard.spec.ts + overdue-contact-updates.spec.ts (see
  // `// spec: CAD-028`).

  // CAD-029 (recent-activity summary) migrated to E2E:
  // contact-direction.spec.ts (see `// spec: CAD-029`).

  // CAD-030 (tasks section: live first, history on demand) migrated to E2E:
  // contact-tasks.spec.ts (see `// spec: CAD-030`).

  // CAD-031 (add manual tasks of three kinds) migrated to E2E:
  // contact-tasks.spec.ts (see `// spec: CAD-031`).

  // CAD-033 (unlink is the only in-CRM mutation of a linked task) migrated
  // to E2E: contact-tasks.spec.ts (see `// spec: CAD-033`).
]

// All 22 CON, 14 DSH, and 22 CAD verifier then-items have migrated to E2E
// (contacts / contact-navigation / contact-merge / birthdays / dashboard /
// navigation / error-boundary / overdue-contact-updates / contact-direction /
// contact-tasks .spec.ts); what remains is only the CON-042[0] + DSH-004[2]
// judge items (2 rows). The classification is now an index-faithful SUBSET of
// the spec, guarded by grade.test.ts's EXPECTED_ROWS rather than a total-count
// check. The constant below is no longer asserted anywhere and is retained
// only until PR4 removes the residual verifier machinery.
export const CLASSIFICATION_ITEM_COUNT = 2

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
