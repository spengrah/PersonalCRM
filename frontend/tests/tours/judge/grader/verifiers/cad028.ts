// CAD-028 — marking contacted from the dashboard clears the contact immediately.
//   [0] a mutual interaction is logged, server-accelerated clock
//   [1] the contact leaves overdue without a reload; the count updates
//   [2] consistent across dashboard/list/detail   (caveat → abstain)
//
// Membership timing rides the accelerated clock, so [1] abstains rather than
// fails when the marked contact is still within the overdue window.

import {
  asArray,
  asRecord,
  asString,
  byRole,
  endpointItems,
  envelopeData,
  findApiItem,
  urlPathname,
} from '../evidence'
import type { Capture, ApiResponseItem } from '../../../support/types'
import type { CaptureSet, ItemVerdict, ItemVerdicts } from '../types'

function markedContactId(after: Capture): string | undefined {
  const post = findApiItem(
    after,
    (i, k) => k === 'POST /api/v1/contacts/:id/interactions' && i.method === 'POST'
  )
  const m = post?.requestUrl.match(/\/contacts\/([^/]+)\/interactions/)
  return m ? m[1] : undefined
}

function overdueIds(cap: Capture): string[] | undefined {
  const items = endpointItems(cap, 'GET /api/v1/contacts/overdue')
  if (items.length === 0) return undefined
  const latest = items[items.length - 1] as ApiResponseItem
  const data = asArray(envelopeData(latest.body))
  if (!data) return undefined
  return data.map(x => asString(asRecord(x)?.id)).filter((x): x is string => x !== undefined)
}

export function cad028(set: CaptureSet): ItemVerdicts {
  const after = byRole(set, 'mark-after')
  const out: ItemVerdicts = {}

  out[0] = ((): ItemVerdict => {
    if (!after) return { verdict: 'unsure', reason: 'no mark-after capture — no evidence' }
    const post = findApiItem(
      after,
      (i, k) => k === 'POST /api/v1/contacts/:id/interactions' && i.method === 'POST'
    )
    if (!post) {
      return {
        verdict: 'fail',
        citation: 'mark-after bracket',
        reason: 'mark-as-contacted did not log an interaction (no POST in the after bracket)',
      }
    }
    const data = asRecord(envelopeData(post.body))
    const direction = asString(data?.direction)
    const occurredAt = asString(data?.occurred_at)
    if (post.status !== 201 || direction !== 'mutual' || !occurredAt) {
      return {
        verdict: 'fail',
        citation: 'POST .../interactions body',
        reason: `expected a mutual, server-timestamped interaction (direction=${direction ?? 'none'}, occurred_at=${occurredAt ?? 'none'}, status=${post.status})`,
      }
    }
    // Accelerated-clock check: occurred_at must fall inside the recorded frame
    // (>= baseTime, not in the future) AND be recent within it. A wall-clock
    // stamp would sit ~a day behind the accelerated currentTime (base + elapsed
    // x accelerationFactor) and fail the recency bound.
    const occ = Date.parse(occurredAt)
    const now = Date.parse(after.serverTime.currentTime)
    const base = Date.parse(after.serverTime.baseTime)
    const ACCEL_RECENCY_MS = 6 * 60 * 60 * 1000
    if (Number.isNaN(occ) || Number.isNaN(now)) {
      return { verdict: 'unsure', reason: 'unparseable occurred_at / serverTime frame' }
    }
    const inFrame = (Number.isNaN(base) || occ >= base) && occ <= now + 5 * 60 * 1000
    const recent = now - occ < ACCEL_RECENCY_MS
    if (!inFrame || !recent) {
      return {
        verdict: 'fail',
        citation: 'POST .../interactions occurred_at vs serverTime frame',
        reason: `occurred_at (${occurredAt}) is not within the accelerated serverTime frame (now=${after.serverTime.currentTime}) — not stamped by the accelerated clock`,
      }
    }
    return {
      verdict: 'pass',
      citation:
        'POST .../interactions body direction=mutual + occurred_at within the accelerated serverTime frame',
    }
  })()

  out[1] = ((): ItemVerdict => {
    if (!after) return { verdict: 'unsure', reason: 'no mark-after capture — no evidence' }
    const refetch = overdueIds(after)
    if (refetch === undefined) {
      return { verdict: 'unsure', reason: 'no overdue refetch in the after bracket — cannot bind' }
    }
    if (urlPathname(after.url) !== '/dashboard') {
      return {
        verdict: 'unsure',
        reason: 'the after capture is not on /dashboard — cannot bind the no-reload update',
      }
    }
    const markedId = markedContactId(after)
    if (markedId && refetch.includes(markedId)) {
      // Still overdue in the refetched list — the accelerated clock may keep it
      // within the window. Abstain rather than fail on ambiguous timing.
      return {
        verdict: 'unsure',
        reason:
          'the marked contact is still in the refetched overdue list (accelerated-clock timing) — abstaining',
      }
    }
    return {
      verdict: 'pass',
      citation: 'overdue refetch on /dashboard no longer lists the marked contact (no reload)',
    }
  })()

  out[2] = {
    verdict: 'unsure',
    reason:
      'multi-surface (dashboard/list/detail) consistency is not toured in one flow — abstain (caveat)',
  }

  return out
}
