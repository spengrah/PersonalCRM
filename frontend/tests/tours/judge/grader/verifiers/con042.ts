// CON-042 — deleting a contact requires explicit confirmation.
//   [0] confirmation prompt warns the action cannot be undone  → JUDGE (not here)
//   [1] only on confirmation is the contact deleted
//   [2] on success returned to the contact list
//
// [0] is the one clearly judge-only item and is graded by the LLM judge over
// dialogs[].message; the verifier owns [1] and [2].

import { byRole, findApiItem, urlPathname } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

export function con042(set: CaptureSet): ItemVerdicts {
  const afterDismiss = byRole(set, 'after-dismiss')
  const afterAccept = byRole(set, 'after-accept')
  const out: ItemVerdicts = {}

  // [1] dismiss keeps the contact live (probe GET → 200); accept deletes it
  // (DELETE 204 + probe GET → 404).
  out[1] = ((): ItemVerdict => {
    const dismissProbe = afterDismiss
      ? findApiItem(afterDismiss, (i, k) => i.probe === true && k === 'GET /api/v1/contacts/:id')
      : undefined
    const del = afterAccept
      ? findApiItem(afterAccept, (_i, k) => k === 'DELETE /api/v1/contacts/:id')
      : undefined
    const acceptProbe = afterAccept
      ? findApiItem(afterAccept, (i, k) => i.probe === true && k === 'GET /api/v1/contacts/:id')
      : undefined
    if (!dismissProbe && !del && !acceptProbe) {
      return { verdict: 'unsure', reason: 'no delete probes captured — no evidence' }
    }
    const dismissLive = dismissProbe?.status === 200
    const deleted = del?.status === 204 && acceptProbe?.status === 404
    if (dismissLive && deleted) {
      return {
        verdict: 'pass',
        citation: 'dismiss probe GET 200; accept DELETE 204 + probe GET 404',
      }
    }
    if (dismissProbe && !dismissLive) {
      return {
        verdict: 'fail',
        citation: 'after-dismiss probe status',
        reason: 'contact was deleted WITHOUT confirmation (dismiss probe not 200)',
      }
    }
    if ((del || acceptProbe) && !deleted) {
      return {
        verdict: 'fail',
        citation: 'after-accept DELETE/probe status',
        reason: 'confirmed delete did not remove the contact (no 204 + 404)',
      }
    }
    return { verdict: 'unsure', reason: 'partial delete evidence — cannot bind' }
  })()

  // [2] after confirmed delete, back on the contact list.
  out[2] = ((): ItemVerdict => {
    if (!afterAccept) return { verdict: 'unsure', reason: 'no after-accept capture — no evidence' }
    return urlPathname(afterAccept.url) === '/contacts'
      ? { verdict: 'pass', citation: 'after-accept url pathname /contacts' }
      : {
          verdict: 'fail',
          citation: 'after-accept url',
          reason: 'not returned to the contact list after delete',
        }
  })()

  return out
}
