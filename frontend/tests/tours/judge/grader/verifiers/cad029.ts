// CAD-029 — recent activity summarizes the direction timestamps + pending reply.
//   [0] last outreach shown when it exists
//   [1] last response shown when it exists
//   [2] awaiting-reply indicator while a follow-up pends  (provider → abstain)
//   [3] none → explicit no-recent-activity state
//
// Each capture is a detail page for an API-selected contact in a specific state
// (outreach / response / none). The verifier binds each labeled line's presence
// to the corresponding detail-body signal.

import {
  asRecord,
  asString,
  ariaTextIncludes,
  byRole,
  envelopeData,
  findApiItem,
} from '../evidence'
import type { Capture } from '../../../support/types'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

function detailBody(cap: Capture): Record<string, unknown> | undefined {
  const item = findApiItem(cap, (_i, k) => k === 'GET /api/v1/contacts/:id')
  return asRecord(envelopeData(item?.body))
}

export function cad029(set: CaptureSet): ItemVerdicts {
  const outreach = byRole(set, 'activity-outreach')
  const response = byRole(set, 'activity-response')
  const none = byRole(set, 'activity-none')
  const out: ItemVerdicts = {}

  // The labeled line's presence must AGREE with the body signal: shown iff the
  // timestamp exists. A mismatch either direction is a defect (not shown when it
  // exists; shown when it doesn't). Only when neither is present do we abstain
  // (the selected contact genuinely lacks the signal — nothing to prove).
  const activityLine = (cap: Capture, field: string, label: string): ItemVerdict => {
    const hasData = asString(detailBody(cap)?.[field]) !== undefined
    const shown = ariaTextIncludes(cap.aria, label)
    if (hasData && shown)
      return { verdict: 'pass', citation: `'${label}' shown with ${field} present` }
    if (hasData && !shown) {
      return {
        verdict: 'fail',
        citation: 'recent-activity block',
        reason: `${field} exists but the '${label}' line is not shown`,
      }
    }
    if (!hasData && shown) {
      return {
        verdict: 'fail',
        citation: 'recent-activity block',
        reason: `the '${label}' line is shown but ${field} is absent from the body`,
      }
    }
    return {
      verdict: 'unsure',
      reason: `the selected contact has no ${field} — cannot prove the shown-when-exists case`,
    }
  }

  out[0] = outreach
    ? activityLine(outreach, 'last_outreach_at', 'Last outreach:')
    : { verdict: 'unsure', reason: 'no outreach-state capture — no evidence' }

  out[1] = response
    ? activityLine(response, 'last_response_at', 'Last response:')
    : { verdict: 'unsure', reason: 'no response-state capture — no evidence' }

  out[2] = {
    verdict: 'unsure',
    reason:
      'the awaiting-reply indicator needs has_pending_followup, which is provider-driven and absent from a provider-less sweep — abstain (skip-list)',
  }

  out[3] = ((): ItemVerdict => {
    if (!none) return { verdict: 'unsure', reason: 'no none-state capture — no evidence' }
    const body = detailBody(none)
    const noSignals =
      asString(body?.last_outreach_at) === undefined &&
      asString(body?.last_response_at) === undefined &&
      body?.has_pending_followup !== true
    const shown = ariaTextIncludes(none.aria, 'No recent activity')
    if (!noSignals) {
      return {
        verdict: 'unsure',
        reason: 'the selected contact unexpectedly has an activity signal — abstain',
      }
    }
    return shown
      ? { verdict: 'pass', citation: "'No recent activity' shown with no direction signals" }
      : {
          verdict: 'fail',
          citation: 'recent-activity block',
          reason: 'no direction signals but the no-recent-activity state is not shown',
        }
  })()

  return out
}
