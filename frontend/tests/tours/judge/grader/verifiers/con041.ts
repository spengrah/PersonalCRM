// CON-041 — action parameters trigger once and are consumed.
//   [0] the action runs once (edit mode opens, or the merge modal opens)
//   [1] the parameter is stripped from the URL

import { byNoteIncludes, findByRoleName, urlQuery } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

export function con041(set: CaptureSet): ItemVerdicts {
  const editCap = byNoteIncludes(set, 'action=edit')
  const mergeCap = byNoteIncludes(set, 'action=merge')
  const out: ItemVerdicts = {}

  // [0] the right surface opened: edit → Edit Contact heading; merge → Merge Contacts.
  out[0] = ((): ItemVerdict => {
    if (!editCap && !mergeCap)
      return { verdict: 'unsure', reason: 'no action captures — no evidence' }
    const editOk = editCap
      ? findByRoleName(editCap.aria, 'heading', 'Edit Contact') !== undefined
      : undefined
    const mergeOk = mergeCap
      ? findByRoleName(mergeCap.aria, 'heading', 'Merge Contacts') !== undefined
      : undefined
    if (editOk === false || mergeOk === false) {
      return {
        verdict: 'fail',
        citation: 'action capture aria',
        reason: 'the action did not open its expected surface',
      }
    }
    return { verdict: 'pass', citation: "aria 'Edit Contact' / 'Merge Contacts' heading" }
  })()

  // [1] no residual action= param in either url (refresh/back must not re-trigger).
  out[1] = ((): ItemVerdict => {
    const caps = [editCap, mergeCap].filter((c): c is NonNullable<typeof c> => c !== undefined)
    if (caps.length === 0) return { verdict: 'unsure', reason: 'no action captures — no evidence' }
    const leaked = caps.find(c => 'action' in urlQuery(c.url))
    return leaked
      ? {
          verdict: 'fail',
          citation: `${leaked.note} url`,
          reason: 'action= parameter was NOT stripped from the URL',
        }
      : { verdict: 'pass', citation: 'no action= param remains on either url' }
  })()

  return out
}
