// CON-040 — keyboard navigation drives the contact detail page.
//   [0] left/right move prev/next, disabled at the boundaries (verifier† caveat)
//   [1] arrows inert while editing OR focus in an input
//   [2] Enter opens edit mode
//   [3] Escape discards edit, or returns to the list (context preserved)

import { byRole, findByRoleName, urlPathname, urlQuery } from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

export function con040(set: CaptureSet): ItemVerdicts {
  const viewBefore = byRole(set, 'view-before')
  const arrowRight = byRole(set, 'arrow-right-next')
  const arrowLeft = byRole(set, 'arrow-left-prev')
  const boundaryFirst = byRole(set, 'boundary-first')
  const inputFocusInert = byRole(set, 'input-focus-inert')
  const enterEdit = byRole(set, 'enter-edit')
  const arrowEditInert = byRole(set, 'arrow-edit-inert')
  const escapeDiscard = byRole(set, 'escape-discard')
  const escapeToList = byRole(set, 'escape-to-list')

  const out: ItemVerdicts = {}

  // [0] prev/next movement + FIRST boundary. The doctor flips boundary-first's
  // `Previous contact` disabled → fail. The last boundary (Next disabled at the
  // last contact) is uncaptured → the verifier abstains on that half (caveat).
  out[0] = ((): ItemVerdict => {
    if (boundaryFirst) {
      const prevNode = findByRoleName(boundaryFirst.aria, 'button', 'Previous contact')
      if (prevNode && prevNode.disabled !== true) {
        return {
          verdict: 'fail',
          citation: "aria button 'Previous contact' at the first boundary",
          reason: 'the Previous nav is NOT disabled at the first-contact boundary',
        }
      }
    }
    const moved =
      viewBefore &&
      arrowRight &&
      arrowLeft &&
      urlPathname(viewBefore.url) !== urlPathname(arrowRight.url) &&
      urlPathname(arrowRight.url) !== urlPathname(arrowLeft.url)
    const firstBoundaryDisabled =
      boundaryFirst !== undefined &&
      findByRoleName(boundaryFirst.aria, 'button', 'Previous contact')?.disabled === true
    if (moved && firstBoundaryDisabled) {
      return {
        verdict: 'unsure',
        citation: "url deltas + aria 'Previous contact' [disabled]",
        reason:
          'prev/next movement and the FIRST boundary (Previous disabled) are proven; the last ' +
          'boundary (Next disabled at the last contact) is not captured — abstaining on that half.',
      }
    }
    return { verdict: 'unsure', reason: 'missing view/arrow/boundary captures — no evidence' }
  })()

  // [1] arrows inert: edit-mode arrow leaves the url unchanged (== enter-edit),
  // and an input-focus arrow leaves the url unchanged (== boundary-first).
  out[1] = ((): ItemVerdict => {
    const editInert = enterEdit && arrowEditInert && arrowEditInert.url === enterEdit.url
    const focusInert = boundaryFirst && inputFocusInert && inputFocusInert.url === boundaryFirst.url
    if (editInert === undefined && focusInert === undefined) {
      return { verdict: 'unsure', reason: 'missing inert-arrow captures — no evidence' }
    }
    if (editInert === false || focusInert === false) {
      return {
        verdict: 'fail',
        citation: 'arrow-edit-inert.url / input-focus-inert.url',
        reason: 'an arrow press changed the url while editing or while focus was in an input',
      }
    }
    return {
      verdict: 'pass',
      citation: 'url unchanged across the edit-mode and input-focus arrow presses',
    }
  })()

  // [2] Enter opens edit mode: the enter-edit capture shows the Edit Contact heading.
  out[2] = ((): ItemVerdict => {
    if (!enterEdit) return { verdict: 'unsure', reason: 'no enter-edit capture — no evidence' }
    const heading = findByRoleName(enterEdit.aria, 'heading', 'Edit Contact')
    return heading
      ? { verdict: 'pass', citation: "aria heading 'Edit Contact'" }
      : {
          verdict: 'fail',
          citation: 'enter-edit aria',
          reason: 'Enter did not open edit mode (no Edit Contact heading)',
        }
  })()

  // [3] Escape discards edit (back to view) OR returns to the list with context
  // preserved (pathname /contacts AND the sort=cadence&order=desc query kept).
  out[3] = ((): ItemVerdict => {
    const discardOk =
      escapeDiscard !== undefined &&
      findByRoleName(escapeDiscard.aria, 'button', 'Edit') !== undefined
    if (!escapeToList) {
      return discardOk
        ? { verdict: 'unsure', reason: 'edit-discard proven; escape-to-list not captured' }
        : { verdict: 'unsure', reason: 'missing escape captures — no evidence' }
    }
    const q = urlQuery(escapeToList.url)
    const listOk =
      urlPathname(escapeToList.url) === '/contacts' && q.sort === 'cadence' && q.order === 'desc'
    if (listOk && discardOk) {
      return {
        verdict: 'pass',
        citation: 'escape-discard back to view; escape-to-list /contacts?sort=cadence&order=desc',
      }
    }
    if (!listOk) {
      return {
        verdict: 'fail',
        citation: 'escape-to-list url',
        reason:
          'Escape did not return to /contacts with the sort=cadence&order=desc context preserved',
      }
    }
    return { verdict: 'unsure', reason: 'escape-to-list proven; edit-discard not captured' }
  })()

  return out
}
