// CAD-031 — users can add manual tasks of three kinds.
//   [0] kind chosen from reach-out / send / reminder
//   [1] task text required (submit disabled until text non-empty)
//   [2] created task appears in live tasks   (provider → abstain)
//
// [0]/[1] are proven WITHOUT submitting (the create needs a Todoist provider, so
// [2] is skip-listed): the modal's kind picker + the submit's disabled→enabled
// transition are captured from the empty-text and filled-text modal states.

import { byRole, findByRoleName } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

const KINDS = ['Reach out', 'Send', 'Reminder']

export function cad031(set: CaptureSet): ItemVerdicts {
  const empty = byRole(set, 'add-task-empty')
  const filled = byRole(set, 'add-task-filled')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    if (!empty) return { verdict: 'unsure', reason: 'no add-task modal capture — no evidence' }
    const missing = KINDS.filter(k => !findByRoleName(empty.aria, 'button', k))
    return missing.length === 0
      ? { verdict: 'pass', citation: `Task type kind picker: ${KINDS.join(' / ')}` }
      : {
          verdict: 'fail',
          citation: 'add-task kind picker',
          reason: `the kind picker is missing: ${missing.join(', ')}`,
        }
  })()

  out[1] = ((): ItemVerdict => {
    if (!empty || !filled) {
      return {
        verdict: 'unsure',
        reason: 'missing the empty-text and/or filled-text modal capture',
      }
    }
    const emptyDisabled = findByRoleName(empty.aria, 'button', 'Add Task')?.disabled === true
    const filledEnabled = findByRoleName(filled.aria, 'button', 'Add Task')?.disabled !== true
    if (emptyDisabled && filledEnabled) {
      return {
        verdict: 'pass',
        citation: "'Add Task' submit disabled with empty text, enabled after typing",
      }
    }
    return {
      verdict: 'fail',
      citation: 'add-task submit disabled state',
      reason: `text-required not enforced (empty=${emptyDisabled ? 'disabled' : 'enabled'}, filled=${filledEnabled ? 'enabled' : 'disabled'})`,
    }
  })()

  out[2] = {
    verdict: 'unsure',
    reason:
      'the create needs a Todoist provider (the submit errors without one) — abstain (skip-list)',
  }

  return out
}
