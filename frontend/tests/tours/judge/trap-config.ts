// The committed trap-mutation config — the live detection self-test's inputs
// (spec "trap-as-transformation"). Each trap names a behavior + then-item the
// tours RELIABLY capture AND that the JUDGE grades (the residue is CON-042[0] +
// DSH-004[2]), plus a single-point mutation that manufactures a KNOWN fail on
// that item AND is visible in the JUDGE-PROJECTED evidence.
//
// Two hard constraints on every entry (both guarded by trap-config.test.ts):
//   1. `{ targetBehavior, targetItem }` MUST be a judge-graded residue item
//      (`judgeItemsFor(targetBehavior)`) — a behavior with no judge residue
//      produces `items: []` and the judge short-circuits, never failing.
//   2. `mutation.op` MUST change evidence the judge lane PROJECTS — the prompt
//      renders url/aria/api/serverTime/dialogs but NOT `Capture.fields`, so
//      `set_field` is INVISIBLE to the judge and MUST NOT be used. Use
//      `blank_dialog` / `set_json_field` / `remove_aria_subtree`.
//
// N is kept small (2–3) to bound per-round judge quota. Adding/adjusting a trap
// is the maintainer recipe that replaces the retired doctored corpus.

import type { Mutation } from './mutation'

export interface TrapSpec {
  // Stable id for the self-test report / trace correlation.
  id: string
  // The judge-graded behavior whose captures this trap doctors.
  targetBehavior: string
  // The then-item (index) the trap manufactures a fail on — must be judge residue.
  targetItem: number
  // The single-point doctoring applied to the round's fresh captures.
  mutation: Mutation
  // Why this trap manufactures a fail on the target item (for reviewers).
  note: string
}

export const TRAPS: TrapSpec[] = [
  {
    id: 'trap-con-042-blank-dialog',
    targetBehavior: 'CON-042',
    targetItem: 0,
    mutation: { op: 'blank_dialog' },
    note:
      'Blanks the delete-confirm dialog message ("...cannot be undone."). The judge must fail ' +
      'CON-042[0] — the confirmation no longer warns the action is irreversible.',
  },
  {
    id: 'trap-dsh-004-stale-reason',
    targetBehavior: 'DSH-004',
    targetItem: 2,
    mutation: {
      op: 'set_json_field',
      // The DSH-004 group carries both a `loading` and an `error` capture; only
      // the error one holds the overdue endpoint. Target it by pair role so the
      // mutation lands deterministically regardless of capture file order (an
      // index-0 default lands on whichever capture readdir returned first).
      role: 'error',
      endpoint: 'GET /api/v1/contacts/overdue',
      path: ['error', 'message'],
      value: 'database connection refused',
      // EVERY 500 of the retried failure bracket — not just the final one.
      // Doctoring only the last 500 left the earlier retries' reason still
      // matching the aria error state, so a single judge call could rationalize
      // the shown reason as faithful to one of the undoctored 500s and returned
      // `unsure` — a deterministic missed trap (gh #708). Rewriting all of them
      // makes the contradiction unambiguous: no response carries the reason the
      // aria still shows, so DSH-004[2]'s faithfulness fails cleanly. Dynamic
      // (status >= 500) rather than a fixed index because a real capture may
      // prepend a warm 200 hit before the retries.
      itemMatch: 'all-errors',
    },
    note:
      'Rewrites every overdue-fetch 500 reason so none matches the reason shown in the aria ' +
      'error state. The judge must fail DSH-004[2] — the shown reason is not faithful.',
  },
]
