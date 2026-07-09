// CAD-030 — the tasks section shows live work first and history on demand.
//   [0] follow-up first (pending indicator), then manual  (provider → abstain)
//   [1] each task badge from kind+lifecycle                (provider → abstain)
//   [2] completed collapsed behind a toggle with count     (provider → abstain)
//   [3] no tasks → empty state invites adding
//
// [0]/[1]/[2] need provider-seeded tasks a provider-less sweep cannot reach →
// skip-list abstain. [3]'s empty state IS reachable and is graded.

import { ariaTextIncludes, byRole } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

export function cad030(set: CaptureSet): ItemVerdicts {
  const tasksEmpty = byRole(set, 'tasks-empty')
  const out: ItemVerdicts = {}

  out[0] = {
    verdict: 'unsure',
    reason:
      'needs provider-seeded follow-up + manual tasks a provider-less sweep cannot reach — abstain (skip-list)',
  }
  out[1] = {
    verdict: 'unsure',
    reason: 'needs seeded task rows to grade the kind+lifecycle badge — abstain (skip-list)',
  }
  out[2] = {
    verdict: 'unsure',
    reason: 'needs seeded completed tasks to grade the collapse toggle — abstain (skip-list)',
  }

  out[3] = ((): ItemVerdict => {
    if (!tasksEmpty) return { verdict: 'unsure', reason: 'no tasks-section capture — no evidence' }
    return ariaTextIncludes(tasksEmpty.aria, 'No tasks yet')
      ? { verdict: 'pass', citation: "Tasks section 'No tasks yet' empty state" }
      : {
          verdict: 'fail',
          citation: 'tasks section aria',
          reason: 'the no-tasks state does not render the empty-state invitation',
        }
  })()

  return out
}
