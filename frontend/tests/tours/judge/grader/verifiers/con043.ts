// CON-043 — the merge flow keeps the current contact and archives the source.
//   [0] current marked kept (Keeping badge); pick source from a selector that excludes the target
//   [1] selecting a source loads a preview
//   [2] conflicting fields toggle, defaulting to keep target
//   [3] merged name editable, with source quick-fill
//   [4] cannot submit before source / while preview loading / while merge in flight
//   [5] outcome reported and auto-dismissed (success-wording faithfulness → judge if unbindable)

import {
  asRecord,
  asString,
  ariaTextIncludes,
  byRole,
  findAllAria,
  findApiItem,
  findByRoleName,
} from '../evidence'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'
import type { AriaNode, Capture } from '../../../support/types'

// The kept contact's name — the level-3 heading in the merge modal.
function keptContactName(capture: Capture): string | undefined {
  return findAllAria(capture.aria, n => n.role === 'heading' && n.level === 3)[0]?.name
}

function submitDisabled(capture: Capture): boolean | undefined {
  const submit = findByRoleName(capture.aria, 'button', 'Merge Contacts')
  return submit ? submit.disabled === true : undefined
}

export function con043(set: CaptureSet): ItemVerdicts {
  const open = byRole(set, 'open')
  const selectorOpen = byRole(set, 'selector-open')
  const previewLoading = byRole(set, 'preview-loading')
  const previewLoaded = byRole(set, 'preview-loaded')
  const nameQuickfilled = byRole(set, 'name-quickfilled')
  const nameEdited = byRole(set, 'name-edited')
  const inFlight = byRole(set, 'in-flight')
  const after = byRole(set, 'after')
  const outcomeReported = byRole(set, 'outcome-reported')
  const dismissed = byRole(set, 'dismissed')
  const out: ItemVerdicts = {}

  // [0] Keeping badge on the current contact + the selector excludes the target.
  out[0] = ((): ItemVerdict => {
    const keptBadge =
      (previewLoaded && ariaTextIncludes(previewLoaded.aria, 'Keeping')) ||
      (open && ariaTextIncludes(open.aria, 'Keeping'))
    const targetName = open ? keptContactName(open) : undefined
    let excludes: boolean | undefined
    if (selectorOpen && targetName) {
      excludes = findAllAria(selectorOpen.aria, n => n.name === targetName).length === 0
    }
    if (keptBadge === undefined && excludes === undefined) {
      return { verdict: 'unsure', reason: 'no merge-modal captures — no evidence' }
    }
    if (keptBadge === false) {
      return {
        verdict: 'fail',
        citation: 'merge modal aria',
        reason: 'no visible "Keeping" badge on the current contact',
      }
    }
    if (excludes === false) {
      return {
        verdict: 'fail',
        citation: 'selector-open aria',
        reason: 'the target appears as a selectable merge candidate (selector does not exclude it)',
      }
    }
    return { verdict: 'pass', citation: 'Keeping badge + selector excludes the target name' }
  })()

  // [1] selecting a source loads a preview (GET /merge/preview + Will Be Merged).
  out[1] = ((): ItemVerdict => {
    if (!previewLoaded)
      return { verdict: 'unsure', reason: 'no preview-loaded capture — no evidence' }
    const previewReq = findApiItem(
      previewLoaded,
      (_i, k) => k === 'GET /api/v1/contacts/:id/merge/preview'
    )
    const willBeMerged = findByRoleName(previewLoaded.aria, 'heading', 'Will Be Merged')
    return previewReq && willBeMerged
      ? { verdict: 'pass', citation: 'GET /merge/preview + aria heading "Will Be Merged"' }
      : {
          verdict: 'fail',
          citation: 'preview-loaded evidence',
          reason: 'selecting a source did not load a preview',
        }
  })()

  // [2] conflict toggle present + POST field_selections default to target.
  out[2] = ((): ItemVerdict => {
    const toggle = previewLoaded
      ? findByRoleName(previewLoaded.aria, 'heading', 'Resolve Conflicts')
      : undefined
    const post = after
      ? findApiItem(after, (i, k) => k === 'POST /api/v1/contacts/:id/merge' && i.method === 'POST')
      : undefined
    const selections = asRecord(asRecord(post?.requestBody)?.field_selections)
    if (!toggle && !selections)
      return { verdict: 'unsure', reason: 'no conflict/merge evidence — no evidence' }
    const allTarget =
      selections !== undefined &&
      Object.values(selections).length > 0 &&
      Object.values(selections).every(v => v === 'target')
    if (toggle && allTarget) {
      return {
        verdict: 'pass',
        citation: 'Resolve Conflicts toggle + field_selections all "target"',
      }
    }
    if (selections && !allTarget) {
      return {
        verdict: 'fail',
        citation: 'POST field_selections',
        reason: 'conflict defaults are not all "target"',
      }
    }
    return { verdict: 'unsure', reason: 'partial conflict-toggle evidence — cannot bind' }
  })()

  // [3] merged name editable + source quick-fill affordance.
  out[3] = ((): ItemVerdict => {
    const quickfill =
      (nameQuickfilled &&
        findByRoleName(nameQuickfilled.aria, 'button', 'use this') !== undefined) ||
      (previewLoaded && findByRoleName(previewLoaded.aria, 'button', 'use this') !== undefined)
    const edited = asString(asRecord(nameEdited?.fields)?.mergedNameInput)
    if (quickfill === undefined && edited === undefined) {
      return { verdict: 'unsure', reason: 'no name-edit captures — no evidence' }
    }
    if (quickfill === false) {
      return {
        verdict: 'fail',
        citation: 'name aria',
        reason: 'no "use this" source quick-fill affordance',
      }
    }
    if (edited !== undefined && edited.trim() === '') {
      return {
        verdict: 'fail',
        citation: 'fields.mergedNameInput',
        reason: 'the merged-name input did not reflect the edit',
      }
    }
    return {
      verdict: 'pass',
      citation: '"use this" quick-fill + fields.mergedNameInput reflects the edit',
    }
  })()

  // [4] submit disabled before source / while preview loading / while in flight.
  out[4] = ((): ItemVerdict => {
    const states: Array<[string, boolean | undefined]> = [
      ['open', open ? submitDisabled(open) : undefined],
      ['preview-loading', previewLoading ? submitDisabled(previewLoading) : undefined],
      ['in-flight', inFlight ? submitDisabled(inFlight) : undefined],
    ]
    const present = states.filter(([, v]) => v !== undefined)
    if (present.length === 0)
      return { verdict: 'unsure', reason: 'no submit-disabled captures — no evidence' }
    const enabled = present.find(([, v]) => v === false)
    return enabled
      ? {
          verdict: 'fail',
          citation: `submit button in the ${enabled[0]} state`,
          reason: 'the merge submit was enabled when it should be disabled',
        }
      : {
          verdict: 'pass',
          citation: 'Merge Contacts submit [disabled] across open/preview-loading/in-flight',
        }
  })()

  // [5] outcome reported + auto-dismissed. Verifier binds the banner presence
  // then absence; the success-WORDING faithfulness is the judge residue.
  out[5] = ((): ItemVerdict => {
    if (!outcomeReported)
      return { verdict: 'unsure', reason: 'no outcome-reported capture — no evidence' }
    const banner = bannerText(outcomeReported.aria)
    const stillShown = dismissed ? bannerText(dismissed.aria) !== undefined : undefined
    if (banner === undefined) {
      return {
        verdict: 'fail',
        citation: 'outcome-reported aria',
        reason: 'no success banner reported after the merge',
      }
    }
    if (stillShown === true) {
      return {
        verdict: 'fail',
        citation: 'dismissed aria',
        reason: 'the success banner did not auto-dismiss',
      }
    }
    return { verdict: 'pass', citation: `success banner shown ("${banner}") then auto-dismissed` }
  })()

  return out
}

function bannerText(aria: AriaNode): string | undefined {
  return findAllAria(aria, n => {
    const t = (n.text ?? n.name ?? '').toLowerCase()
    return t.includes('merged successfully') || t.includes('merge')
  }).find(n => (n.text ?? n.name ?? '').toLowerCase().includes('merged successfully'))?.text
}
