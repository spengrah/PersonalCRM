// The explicit skip-list surfaced in the advisory coverage report (design D5):
// the first-cut behaviors / clauses NOT toured, each with a reason. This is
// harness-local and advisory — it files no issues and gates no CI. It is NOT a
// repo-wide scanner (that is Piece 3); it enumerates only the scoped domains'
// deliberate omissions.

export interface SkipEntry {
  /** A whole behavior id (proposed), or a `BEHAVIOR[clause]` for a single clause. */
  id: string
  reason: string
}

// Whole behaviors skipped because they are status: proposed (they describe
// behavior that does not hold yet), per the arc's current-ux-only rule.
export const PROPOSED_SKIPS: SkipEntry[] = [
  {
    id: 'CON-046',
    reason:
      'status: proposed — a failed list/detail mark-as-contacted surfaces no user-facing error today',
  },
  {
    id: 'DSH-006',
    reason:
      'status: proposed — a failed dashboard mark-as-contacted is logged to the console only, no user-facing error',
  },
  {
    id: 'DSH-009',
    reason:
      'status: proposed — import-link and cadence-changing edits do not refresh the overdue widget today (intended-but-broken)',
  },
]

// Individual clauses of toured behaviors that a provider-less local sweep or a
// single-tour flow cannot reach; the verifier abstains (unsure) on these.
export const CLAUSE_SKIPS: SkipEntry[] = [
  {
    id: 'DSH-005[1]',
    reason:
      'multi-surface: only interaction:created (mark-contacted) is dashboard-reachable; merge / meeting-note-resolve are other-surface flows',
  },
  {
    id: 'DSH-005[2]',
    reason: 'a cosmetic-edit flow is not toured from the dashboard',
  },
  {
    id: 'DSH-005[3]',
    reason:
      'focus-timing: the refocus / 5-minute-staleTime behavior is not deterministically tourable',
  },
  {
    id: 'CAD-028[2]',
    reason: 'multi-surface: dashboard/list/detail consistency is not toured in one flow',
  },
  {
    id: 'CAD-029[2]',
    reason:
      'Todoist provider: the awaiting-reply indicator needs has_pending_followup (provider-driven)',
  },
  {
    id: 'CAD-030[0]',
    reason:
      'Todoist provider: follow-up-first ordering needs provider-seeded follow-up + manual tasks',
  },
  {
    id: 'CAD-030[1]',
    reason: 'Todoist provider: the kind+lifecycle badge needs seeded task rows',
  },
  {
    id: 'CAD-030[2]',
    reason: 'Todoist provider: the completed-collapse toggle needs seeded completed tasks',
  },
  {
    id: 'CAD-031[2]',
    reason:
      'Todoist provider: the create-success clause needs a configured provider (the submit errors without one)',
  },
  {
    id: 'CAD-033[0]',
    reason: 'Todoist provider: the unlink affordance needs a provider-seeded linked task',
  },
  {
    id: 'CAD-033[1]',
    reason:
      'Todoist provider: proving complete/dismiss live only in the remote app needs provider-seeded tasks',
  },
]

export const SKIP_LIST: SkipEntry[] = [...PROPOSED_SKIPS, ...CLAUSE_SKIPS]
