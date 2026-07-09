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
    // A genuinely missing bracket abstains; a PRESENT bracket lacking its
    // required evidence is a fail (a required datum absent, not "no evidence").
    if (!afterDismiss && !afterAccept) {
      return { verdict: 'unsure', reason: 'no delete brackets captured — no evidence' }
    }
    if (afterAccept && (!del || !acceptProbe)) {
      return {
        verdict: 'fail',
        citation: 'after-accept bracket',
        reason:
          'confirmed-delete evidence absent from the present after-accept bracket (no DELETE 204 + probe GET 404)',
      }
    }
    if (afterDismiss && !dismissProbe) {
      return {
        verdict: 'fail',
        citation: 'after-dismiss bracket',
        reason: 'liveness probe absent from the present after-dismiss bracket',
      }
    }
    const dismissLive = !afterDismiss || dismissProbe?.status === 200
    const deleted = !afterAccept || (del?.status === 204 && acceptProbe?.status === 404)
    if (!dismissLive) {
      return {
        verdict: 'fail',
        citation: 'after-dismiss probe status',
        reason: 'contact was deleted WITHOUT confirmation (dismiss probe not 200)',
      }
    }
    if (!deleted) {
      return {
        verdict: 'fail',
        citation: 'after-accept DELETE/probe status',
        reason: 'confirmed delete did not remove the contact (no 204 + 404)',
      }
    }
    // Both halves are needed to fully bind "only on confirmation is it deleted".
    if (afterDismiss && afterAccept) {
      return {
        verdict: 'pass',
        citation: 'dismiss probe GET 200; accept DELETE 204 + probe GET 404',
      }
    }
    return { verdict: 'unsure', reason: 'only one delete half captured — cannot fully bind' }
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
